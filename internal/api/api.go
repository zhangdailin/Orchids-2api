package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/auth"
	"orchids-api/internal/bolt"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/middleware"
	"orchids-api/internal/orchids"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

type API struct {
	store        *store.Store
	tokenCache   tokencache.Cache
	promptCache  tokencache.PromptCache
	adminUser    string
	adminPass    string
	loginLimiter *middleware.RateLimiter
	config       atomic.Pointer[config.Config]

	// Account check backoff / storm control
	checkMu          sync.Mutex
	checkInFlight    map[int64]bool
	checkFailCount   map[int64]int
	checkNextAllowed map[int64]time.Time
	checkSem         chan struct{}
}

var orchidsGetAccountToken = func(acc *store.Account, cfg *config.Config) (string, error) {
	return orchids.NewFromAccount(acc, cfg).GetToken()
}

var orchidsGetAccountChatToken = func(acc *store.Account, cfg *config.Config) (string, error) {
	return orchids.NewFromAccount(acc, cfg).GetChatToken()
}

var orchidsFetchCredits = orchids.FetchCreditsWithProxy

var boltFetchRootData = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*bolt.RootData, error) {
	client := bolt.NewFromAccount(acc, cfg)
	defer client.Close()
	return client.FetchRootData(ctx)
}

var puterVerifyAccount = func(ctx context.Context, acc *store.Account, cfg *config.Config) error {
	client := puter.NewFromAccount(acc, cfg)
	defer client.Close()
	return client.VerifyAuthToken(ctx)
}

func normalizeGrokVerifyModelID(raw string) string {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "grok-3"
	}
	lower := strings.ToLower(model)
	if lower == "grok" || !strings.HasPrefix(lower, "grok-") {
		return "grok-3"
	}
	return model
}

func isGrokModelNotFound(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "model is not found") || strings.Contains(lower, "model not found")
}

func normalizeWarpTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "warp") {
		return
	}
	acc.RefreshToken = warp.ResolveRefreshToken(acc)
	// Warp 只使用 refresh_token，清理 client_cookie 避免混用
	acc.Token = ""
	acc.ClientCookie = ""
	acc.SessionCookie = ""
}

func normalizeWarpTokenOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	copyAcc := *acc
	if strings.EqualFold(copyAcc.AccountType, "warp") {
		copyAcc.RefreshToken = warp.ResolveRefreshToken(&copyAcc)
		// Warp 对外只暴露 refresh_token，避免把运行时 JWT 当成用户凭据回填到前端。
		copyAcc.Token = ""
		copyAcc.ClientCookie = ""
		copyAcc.SessionCookie = ""
	}
	return &copyAcc
}

// classifyAccountStatusFromError delegates to the centralized errors package.
func classifyAccountStatusFromError(errStr string) string {
	return apperrors.ClassifyAccountStatus(errStr)
}

func httpStatusFromAccountStatus(status string) int {
	switch strings.TrimSpace(status) {
	case "401":
		return http.StatusUnauthorized
	case "403":
		return http.StatusForbidden
	case "404":
		return http.StatusNotFound
	case "429":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func normalizeGrokTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	acc.ClientCookie = grok.NormalizeSSOToken(raw)
	// Grok only needs SSO token; clear unrelated fields.
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.SessionID = ""
	acc.ClientUat = ""
	acc.ProjectID = ""
}

func normalizeOrchidsCredentialInput(acc *store.Account) error {
	if acc == nil || !strings.EqualFold(acc.AccountType, "orchids") {
		return nil
	}

	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(acc.ClientCookie), "Bearer "))
	if raw == "" {
		return nil
	}

	if parsed, ok, err := clerk.ParseOrchidsCookies(raw); err != nil {
		return fmt.Errorf("invalid orchids cookies: %w", err)
	} else if ok {
		acc.ClientCookie = strings.TrimSpace(parsed.ClientCookie)
		acc.ClientUat = strings.TrimSpace(parsed.ClientUat)
		if sessionJWT := strings.TrimSpace(parsed.SessionCookie); sessionJWT != "" {
			acc.SessionCookie = sessionJWT
			if sid, sub := clerk.ParseSessionInfoFromJWT(sessionJWT); sid != "" {
				acc.SessionID = sid
				if acc.UserID == "" {
					acc.UserID = sub
				}
			}
			if strings.TrimSpace(acc.ClientCookie) == "" {
				acc.Token = sessionJWT
			}
		}
		return nil
	}

	if isLikelyJWT(raw) {
		if jwtHasRotatingToken(raw) {
			acc.ClientCookie = raw
			acc.SessionCookie = ""
			acc.SessionID = ""
			acc.Token = ""
			return nil
		}
		acc.Token = raw
		acc.ClientCookie = ""
		acc.SessionCookie = raw
		if sid, sub := clerk.ParseSessionInfoFromJWT(raw); sid != "" {
			acc.SessionID = sid
			if acc.UserID == "" {
				acc.UserID = sub
			}
		}
		return nil
	}

	clientJWT, sessionJWT, err := clerk.ParseClientCookies(raw)
	if err != nil {
		return err
	}
	acc.ClientCookie = clientJWT
	if sessionJWT != "" {
		acc.SessionCookie = sessionJWT
		if sid, sub := clerk.ParseSessionInfoFromJWT(sessionJWT); sid != "" {
			acc.SessionID = sid
			if acc.UserID == "" {
				acc.UserID = sub
			}
		}
	}
	return nil
}

func normalizeAccountOutput(acc *store.Account) *store.Account {
	out := normalizeWarpTokenOutput(acc)
	if out == nil {
		return nil
	}
	if strings.EqualFold(out.AccountType, "grok") {
		out.RefreshToken = ""
		out.SessionCookie = ""
	}
	return out
}

func buildQuotaResponseFields(acc *store.Account) map[string]interface{} {
	fields := map[string]interface{}{
		"quota_limit":     0.0,
		"quota_used":      0.0,
		"quota_remaining": 0.0,
		"quota_mode":      "remaining",
		"quota_unit":      "credits",
		"quota_supported": true,
	}
	if acc == nil {
		return fields
	}

	limit := acc.UsageLimit
	current := acc.UsageCurrent
	if limit < 0 {
		limit = 0
	}
	if current < 0 {
		current = 0
	}

	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "grok":
		limit = grok.InferQuotaLimit(acc)
		remaining := current
		if remaining > limit && limit > 0 {
			limit = remaining
		}
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
		fields["quota_mode"] = "remaining"
		fields["quota_unit"] = "requests"
	case "warp":
		fields["quota_limit"] = limit
		used := current
		if used > limit && limit > 0 {
			used = limit
		}
		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
		fields["quota_mode"] = "used"
		fields["quota_unit"] = "requests"
	case "puter":
		fields["quota_limit"] = 0.0
		fields["quota_used"] = 0.0
		fields["quota_remaining"] = 0.0
		fields["quota_mode"] = "unknown"
		fields["quota_unit"] = "credits"
		fields["quota_supported"] = false
	default:
		fields["quota_limit"] = limit
		remaining := current
		if remaining > limit && limit > 0 {
			remaining = limit
		}
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
	}

	return fields
}

func orchidsCreditsToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	if sessionJWT := strings.TrimSpace(acc.SessionCookie); sessionJWT != "" {
		return sessionJWT
	}
	tok := strings.TrimSpace(acc.Token)
	if tok == "" {
		return ""
	}
	if tok == strings.TrimSpace(acc.ClientCookie) {
		return ""
	}
	return tok
}

func applyOrchidsQuotaFromCredits(acc *store.Account, creditsInfo *orchids.CreditsInfo) {
	if acc == nil || creditsInfo == nil {
		return
	}
	acc.Subscription = strings.ToLower(creditsInfo.Plan)
	acc.UsageCurrent = creditsInfo.Credits
	acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
}

func (a *API) refreshAccountState(ctx context.Context, acc *store.Account) (string, int, error) {
	if acc == nil {
		return "", http.StatusBadRequest, fmt.Errorf("account is nil")
	}

	if strings.EqualFold(acc.AccountType, "warp") {
		cfg := a.config.Load()
		warpClient := warp.NewFromAccount(acc, cfg)
		jwt, err := warpClient.ForceRefreshAccount(ctx)
		if err != nil {
			httpStatus := http.StatusBadRequest
			if code := warp.HTTPStatusCode(err); code >= 400 {
				httpStatus = code
			}
			accountStatus := ""
			if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden || httpStatus == http.StatusTooManyRequests {
				accountStatus = strconv.Itoa(httpStatus)
			}
			return accountStatus, httpStatus, fmt.Errorf("Failed to refresh warp account: %w", err)
		}
		acc.Token = jwt
		warpClient.SyncAccountState()

		limitCtx, limitCancel := context.WithTimeout(ctx, 15*time.Second)
		limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
		limitCancel()
		if limitErr == nil && limitInfo != nil {
			if planTier := strings.ToLower(strings.TrimSpace(limitInfo.PlanTier)); planTier != "" {
				acc.Subscription = planTier
			} else if planName := strings.ToLower(strings.TrimSpace(limitInfo.PlanName)); planName != "" {
				acc.Subscription = planName
			} else if limitInfo.IsUnlimited {
				acc.Subscription = "unlimited"
			} else if strings.TrimSpace(acc.Subscription) == "" {
				acc.Subscription = "free"
			}
			totalLimit := float64(limitInfo.RequestLimit)
			for _, bg := range bonuses {
				totalLimit += float64(bg.RequestCreditsRemaining)
			}
			usedRequests := float64(limitInfo.RequestsUsedSinceLastRefresh)
			acc.UsageLimit = totalLimit
			acc.UsageCurrent = usedRequests
			if limitInfo.NextRefreshTime != "" {
				if t, err := time.Parse(time.RFC3339, limitInfo.NextRefreshTime); err == nil {
					acc.QuotaResetAt = t
				}
			}
		} else if limitErr != nil {
			slog.Warn("Warp quota sync failed after refresh; keeping account available", "account_id", acc.ID, "error", limitErr)
		}
		return "", 0, nil
	}

	if strings.EqualFold(acc.AccountType, "grok") {
		if strings.TrimSpace(acc.ClientCookie) == "" {
			return "", http.StatusBadRequest, fmt.Errorf("Failed to verify grok account: missing sso token")
		}

		cfg := a.config.Load()
		client := grok.New(cfg)

		modelID := normalizeGrokVerifyModelID(acc.AgentMode)
		if modelID != "" && modelID != acc.AgentMode {
			acc.AgentMode = modelID
		}

		verifyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		info, verifyErr := client.VerifyToken(verifyCtx, acc.ClientCookie, modelID)
		cancel()
		if verifyErr != nil && isGrokModelNotFound(verifyErr) && modelID != "grok-3" {
			modelID = "grok-3"
			acc.AgentMode = modelID
			verifyCtx, cancel = context.WithTimeout(ctx, 20*time.Second)
			info, verifyErr = client.VerifyToken(verifyCtx, acc.ClientCookie, modelID)
			cancel()
		}
		if verifyErr != nil {
			status := classifyAccountStatusFromError(verifyErr.Error())
			return status, httpStatusFromAccountStatus(status), fmt.Errorf("Failed to verify grok account: %w", verifyErr)
		}

		if info != nil {
			grok.ApplyQuotaInfo(acc, info)
		}
		return "", 0, nil
	}

	if strings.EqualFold(acc.AccountType, "bolt") {
		if strings.TrimSpace(acc.SessionCookie) == "" && strings.TrimSpace(acc.ClientCookie) == "" {
			return "", http.StatusBadRequest, fmt.Errorf("Failed to verify bolt account: missing session token")
		}
		if strings.TrimSpace(acc.ProjectID) == "" {
			return "", http.StatusBadRequest, fmt.Errorf("Failed to verify bolt account: missing project id")
		}

		rootData, err := boltFetchRootData(ctx, acc, a.config.Load())
		if err != nil {
			status := classifyAccountStatusFromError(err.Error())
			httpStatus := http.StatusBadGateway
			if status != "" {
				httpStatus = httpStatusFromAccountStatus(status)
			}
			return status, httpStatus, fmt.Errorf("Failed to verify bolt account: %w", err)
		}
		bolt.ApplyRootData(acc, rootData)
		return "", 0, nil
	}

	if strings.EqualFold(acc.AccountType, "puter") {
		if strings.TrimSpace(acc.ClientCookie) == "" &&
			strings.TrimSpace(acc.Token) == "" &&
			strings.TrimSpace(acc.SessionCookie) == "" {
			return "", http.StatusBadRequest, fmt.Errorf("Failed to verify puter account: missing auth token")
		}
		if err := puterVerifyAccount(ctx, acc, a.config.Load()); err != nil {
			status := classifyAccountStatusFromError(err.Error())
			httpStatus := http.StatusBadGateway
			if status != "" {
				httpStatus = httpStatusFromAccountStatus(status)
			}
			return status, httpStatus, fmt.Errorf("Failed to verify puter account: %w", err)
		}
		return "", 0, nil
	}

	cfg := a.config.Load()
	proxyFunc := http.ProxyFromEnvironment
	if cfg != nil {
		proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	}

	jwt, err := orchidsGetAccountToken(acc, cfg)
	if err != nil {
		status := classifyAccountStatusFromError(err.Error())
		httpStatus := http.StatusBadRequest
		if status != "" {
			httpStatus = httpStatusFromAccountStatus(status)
		}
		if strings.TrimSpace(acc.ClientCookie) == "" &&
			strings.TrimSpace(acc.SessionCookie) == "" &&
			strings.TrimSpace(acc.Token) == "" {
			httpStatus = http.StatusBadRequest
		}

		// Keep quota display usable for session-only Orchids accounts:
		// even when chat auth cannot be established, the Clerk session JWT may
		// still be enough to read credits.
		quotaSynced := false
		if creditsJWT := orchidsCreditsToken(acc); creditsJWT != "" {
			if sid, sub := clerk.ParseSessionInfoFromJWT(creditsJWT); sub != "" {
				if acc.SessionID == "" && sid != "" {
					acc.SessionID = sid
				}
				if acc.UserID == "" {
					acc.UserID = sub
				}
			}
			if strings.TrimSpace(acc.UserID) != "" {
				if creditsInfo, creditsErr := orchidsFetchCredits(ctx, creditsJWT, acc.UserID, proxyFunc); creditsErr == nil {
					applyOrchidsQuotaFromCredits(acc, creditsInfo)
					quotaSynced = true
				} else {
					slog.Warn("Orchids quota fallback failed after chat auth failure", "account_id", acc.ID, "error", creditsErr)
				}
			}
		}
		if quotaSynced {
			slog.Warn("Orchids chat auth refresh failed but quota sync succeeded; keeping account refresh successful", "account_id", acc.ID, "status", status, "error", err)
			return "", 0, nil
		}
		return status, httpStatus, fmt.Errorf("Failed to refresh account: %w", err)
	}

	acc.Token = strings.TrimSpace(jwt)
	if sid, sub := clerk.ParseSessionInfoFromJWT(acc.Token); sub != "" {
		if acc.SessionID == "" && sid != "" {
			acc.SessionID = sid
		}
		if acc.UserID == "" {
			acc.UserID = sub
		}
	}

	creditsInfo, creditsErr := orchidsFetchCredits(ctx, acc.Token, acc.UserID, proxyFunc)
	if creditsErr != nil {
		status := classifyAccountStatusFromError(creditsErr.Error())
		httpStatus := http.StatusBadRequest
		if status != "" {
			httpStatus = httpStatusFromAccountStatus(status)
		}
		return status, httpStatus, fmt.Errorf("Failed to refresh account: %w", creditsErr)
	}
	applyOrchidsQuotaFromCredits(acc, creditsInfo)
	return "", 0, nil
}

type ExportData struct {
	Version  int             `json:"version"`
	ExportAt time.Time       `json:"export_at"`
	Accounts []store.Account `json:"accounts"`
}

type ImportResult struct {
	Total    int `json:"total"`
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

type CreateKeyResponse struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	KeySuffix string    `json:"key_suffix"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

type UpdateKeyRequest struct {
	Enabled *bool `json:"enabled"`
}

func New(s *store.Store, adminUser, adminPass string, cfg *config.Config) *API {
	a := &API{
		store:        s,
		adminUser:    adminUser,
		adminPass:    adminPass,
		loginLimiter: middleware.NewRateLimiter(5, 15*time.Minute),

		checkInFlight:    map[int64]bool{},
		checkFailCount:   map[int64]int{},
		checkNextAllowed: map[int64]time.Time{},
		checkSem:         make(chan struct{}, 2),
	}
	if cfg != nil {
		a.config.Store(cfg)
	}
	return a
}

func (a *API) SetPromptCache(cache tokencache.PromptCache) {
	a.promptCache = cache
}

func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := middleware.ExtractIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Real-IP"))
	if a.loginLimiter != nil && !a.loginLimiter.Allow(ip) {
		http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !secureCompare(req.Username, a.adminUser) || !secureCompare(req.Password, a.adminPass) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// NOTE: Do not mark cookies as Secure when served over plain HTTP,
	// otherwise browsers will drop the cookie and the Admin UI will appear unable to log in.
	// When behind a TLS-terminating proxy, honor X-Forwarded-Proto.
	isHTTPS := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		auth.InvalidateSessionToken(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(a.config.Load())
	case http.MethodPost:
		// Copy current config, decode into copy, then atomically store
		current := a.config.Load()
		newCfg := *current
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.persistConfig(r.Context(), current, &newCfg); err != nil {
			http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(&newCfg)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleConfigList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	data, err := configPayload(a.config.Load())
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "获取配置失败: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"data": data,
	})
}

func (a *API) HandleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	current := a.config.Load()
	newCfg, err := buildConfigFromPatch(r, current)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "parse request failed: " + err.Error(),
		})
		return
	}
	if err := a.persistConfig(r.Context(), current, newCfg); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "save config failed: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"msg":  "success",
	})
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		accounts, err := a.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if accounts == nil {
			accounts = []*store.Account{}
		}
		normalized := make([]*store.Account, 0, len(accounts))
		for _, acc := range accounts {
			normalized = append(normalized, normalizeAccountOutput(acc))
		}
		json.NewEncoder(w).Encode(normalized)

	case http.MethodPost:
		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = "orchids"
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			normalizeWarpTokenInput(&acc)
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
		} else if acc.ClientCookie != "" {
			if err := normalizeOrchidsCredentialInput(&acc); err != nil {
				http.Error(w, "Invalid client cookie: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if acc.ClientCookie != "" && acc.SessionID == "" && strings.EqualFold(acc.AccountType, "orchids") {
			cfg := a.config.Load()
			proxyFunc := http.ProxyFromEnvironment
			if cfg != nil {
				proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
			}
			info, err := clerk.FetchAccountInfoWithSessionProxy(acc.ClientCookie, acc.SessionCookie, proxyFunc)
			if err != nil {
				applyAccountStatusFromError(&acc, err)
				slog.Warn("Failed to fetch account info, saving without session data", "error", err)
			} else {
				acc.SessionID = info.SessionID
				acc.ClientUat = info.ClientUat
				acc.ProjectID = info.ProjectID
				acc.UserID = info.UserID
				acc.Email = info.Email
				if info.JWT != "" {
					acc.Token = info.JWT
				}
				if info.ClientCookie != "" {
					acc.ClientCookie = info.ClientCookie
				}
			}
		}

		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Error("Failed to create account", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if acc.Enabled && shouldSyncAccountOnCreate(&acc) {
			syncCtx, syncCancel := context.WithTimeout(r.Context(), 25*time.Second)
			accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &acc)
			syncCancel()
			if syncErr != nil {
				slog.Warn("Initial account sync failed", "account_id", acc.ID, "type", acc.AccountType, "error", syncErr)
				if accountStatus != "" {
					acc.StatusCode = accountStatus
					acc.LastAttempt = time.Now()
				}
			} else {
				acc.StatusCode = ""
				acc.LastAttempt = time.Time{}
			}
			if updateErr := a.store.UpdateAccount(r.Context(), &acc); updateErr != nil {
				slog.Warn("Failed to persist initial account sync", "account_id", acc.ID, "type", acc.AccountType, "error", updateErr)
			}
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleAccountByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(path, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	isRefresh := len(parts) > 1 && parts[1] == "refresh"
	isVerify := len(parts) > 1 && parts[1] == "verify"
	isCheck := len(parts) > 1 && parts[1] == "check"
	isUsage := len(parts) > 1 && parts[1] == "usage"

	switch r.Method {
	case http.MethodGet:
		if isRefresh || isVerify {
			http.Error(w, "Deprecated endpoint. Use /api/accounts/{id}/check instead.", http.StatusGone)
			return
		}
		if isUsage {
			acc, err := a.store.GetAccount(r.Context(), id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			resp := map[string]interface{}{
				"account_id":     acc.ID,
				"name":           acc.Name,
				"account_type":   acc.AccountType,
				"subscription":   acc.Subscription,
				"usage_current":  acc.UsageCurrent,
				"usage_limit":    acc.UsageLimit,
				"usage_total":    acc.UsageTotal,
				"quota_reset_at": acc.QuotaResetAt,
			}
			for k, v := range buildQuotaResponseFields(acc) {
				resp[k] = v
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if isCheck {
			// Storm control / backoff: only allow a small number of concurrent checks,
			// and apply exponential backoff per account on failures.
			now := time.Now()
			a.checkMu.Lock()
			if a.checkInFlight[id] {
				a.checkMu.Unlock()
				http.Error(w, "account check already in progress", http.StatusTooManyRequests)
				return
			}
			if next, ok := a.checkNextAllowed[id]; ok && !next.IsZero() && now.Before(next) {
				retryAfter := int(next.Sub(now).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				a.checkMu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "account check backoff", http.StatusTooManyRequests)
				return
			}
			a.checkInFlight[id] = true
			a.checkMu.Unlock()
			defer func() {
				a.checkMu.Lock()
				delete(a.checkInFlight, id)
				a.checkMu.Unlock()
			}()

			// global concurrency limit
			a.checkSem <- struct{}{}
			defer func() { <-a.checkSem }()

			acc, err := a.store.GetAccount(r.Context(), id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}

			checkOK := false
			checkErrStatus := ""
			defer func() {
				a.checkMu.Lock()
				defer a.checkMu.Unlock()
				if checkOK {
					a.checkFailCount[id] = 0
					a.checkNextAllowed[id] = time.Now().Add(3 * time.Second)
					return
				}
				fails := a.checkFailCount[id] + 1
				a.checkFailCount[id] = fails
				d := time.Duration(1<<util.MinInt(fails, 8)) * time.Second
				// For CF/rate-limit style failures, start with a bigger cooldown.
				if checkErrStatus == "403" || checkErrStatus == "429" {
					if d < 60*time.Second {
						d = 60 * time.Second
					}
				}
				if d > 10*time.Minute {
					d = 10 * time.Minute
				}
				a.checkNextAllowed[id] = time.Now().Add(d)
			}()

			accountStatus, httpStatus, refreshErr := a.refreshAccountState(r.Context(), acc)
			if refreshErr != nil {
				checkErrStatus = accountStatus
				if accountStatus != "" {
					acc.StatusCode = accountStatus
					acc.LastAttempt = time.Now()
					if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
						slog.Warn("Failed to persist account refresh status", "account_id", acc.ID, "error", updateErr)
					}
				}
				if httpStatus == 0 {
					httpStatus = http.StatusBadRequest
				}
				http.Error(w, refreshErr.Error(), httpStatus)
				return
			}

			// 刷新/验证成功后清理账号状态
			acc.StatusCode = ""
			acc.LastAttempt = time.Time{}
			checkOK = true

			if err := a.store.UpdateAccount(r.Context(), acc); err != nil {
				http.Error(w, "Failed to save checked account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(normalizeAccountOutput(acc))
			return
		}
		acc, err := a.store.GetAccount(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(normalizeAccountOutput(acc))

	case http.MethodPut:
		existing, err := a.store.GetAccount(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.ID = id
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = existing.AccountType
		}
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = "orchids"
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			normalizeWarpTokenInput(&acc)
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
		} else if acc.ClientCookie != "" {
			if err := normalizeOrchidsCredentialInput(&acc); err != nil {
				http.Error(w, "Invalid client cookie: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		if acc.SessionID == "" {
			acc.SessionID = existing.SessionID
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			if strings.TrimSpace(acc.RefreshToken) == "" {
				acc.RefreshToken = existing.RefreshToken
			}
			if strings.TrimSpace(acc.DeviceID) == "" {
				acc.DeviceID = existing.DeviceID
			}
			if strings.TrimSpace(acc.RequestID) == "" {
				acc.RequestID = existing.RequestID
			}
		}
		if acc.SessionCookie == "" {
			acc.SessionCookie = existing.SessionCookie
		}
		if acc.ClientUat == "" {
			acc.ClientUat = existing.ClientUat
		}
		if acc.ProjectID == "" {
			acc.ProjectID = existing.ProjectID
		}
		if acc.UserID == "" {
			acc.UserID = existing.UserID
		}
		if acc.Email == "" {
			acc.Email = existing.Email
		}
		if !acc.NSFWEnabled && existing.NSFWEnabled {
			acc.NSFWEnabled = true
		}

		if err := a.store.UpdateAccount(r.Context(), &acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	case http.MethodDelete:
		if err := a.store.DeleteAccount(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exportData := ExportData{
		Version:  1,
		ExportAt: time.Now(),
		Accounts: make([]store.Account, len(accounts)),
	}
	for i, acc := range accounts {
		exportData.Accounts[i] = *normalizeAccountOutput(acc)
		exportData.Accounts[i].ID = 0
		exportData.Accounts[i].RequestCount = 0
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=accounts_export.json")
	json.NewEncoder(w).Encode(exportData)
}

func (a *API) HandleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var exportData ExportData
	if err := json.NewDecoder(r.Body).Decode(&exportData); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	result := ImportResult{Total: len(exportData.Accounts)}

	for _, acc := range exportData.Accounts {
		acc.ID = 0
		acc.RequestCount = 0
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = "orchids"
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			normalizeWarpTokenInput(&acc)
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
		} else if acc.ClientCookie != "" {
			if err := normalizeOrchidsCredentialInput(&acc); err != nil {
				slog.Warn("Invalid client cookie in import", "name", acc.Name, "error", err)
				result.Skipped++
				continue
			}
		}
		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Warn("Failed to import account", "name", acc.Name, "error", err)
			result.Skipped++
		} else {
			result.Imported++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func generateApiKey() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, 48)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return "sk-" + string(b), nil
}

func (a *API) HandleKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		keys, err := a.store.ListApiKeys(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(keys)

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}

		fullKey, err := generateApiKey()
		if err != nil {
			slog.Error("Failed to generate api key", "error", err)
			http.Error(w, "failed to generate api key", http.StatusInternalServerError)
			return
		}

		hash := sha256.Sum256([]byte(fullKey))
		hashStr := hex.EncodeToString(hash[:])
		key := store.ApiKey{
			Name:      req.Name,
			KeyHash:   hashStr,
			KeyFull:   fullKey,
			KeyPrefix: "sk-",
			KeySuffix: fullKey[len(fullKey)-4:],
			Enabled:   true,
		}
		if err := a.store.CreateApiKey(r.Context(), &key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(CreateKeyResponse{
			ID:        key.ID,
			Key:       fullKey,
			Name:      key.Name,
			KeyPrefix: key.KeyPrefix,
			KeySuffix: key.KeySuffix,
			Enabled:   key.Enabled,
			CreatedAt: key.CreatedAt,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleKeyByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	idStr := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req UpdateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Enabled == nil {
			http.Error(w, "enabled is required", http.StatusBadRequest)
			return
		}

		if err := a.store.UpdateApiKeyEnabled(r.Context(), id, *req.Enabled); err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		key, err := a.store.GetApiKeyByID(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if key == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(key)

	case http.MethodDelete:
		if err := a.store.DeleteApiKey(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		models, err := a.store.ListModels(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(models)

	case http.MethodPost:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := a.store.CreateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(m)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if id == "" {
		http.Error(w, "Model ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) || err.Error() == "redis: nil" {
				http.Error(w, "Model not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodPut:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.ID = id

		if err := a.store.UpdateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodDelete:
		if err := a.store.DeleteModel(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) SetTokenCache(c tokencache.Cache) {
	a.tokenCache = c
}

func (a *API) HandleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	if !a.cacheTokenCountEnabled() || a.tokenCache == nil {
		// No cache configured
		json.NewEncoder(w).Encode(map[string]interface{}{
			"count":      0,
			"size_bytes": 0,
			"status":     "disabled",
		})
		return
	}

	count, size, err := a.tokenCache.GetStats(r.Context())
	if err != nil {
		http.Error(w, "Failed to get stats: "+err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"count":      count,
		"size_bytes": size,
		"status":     "enabled",
	})
}

func (a *API) HandleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.tokenCache == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := a.tokenCache.Clear(r.Context()); err != nil {
		http.Error(w, "Failed to clear cache: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (a *API) cacheTokenCountEnabled() bool {
	cfg := a.config.Load()
	if cfg == nil {
		return false
	}
	return cfg.CacheTokenCount
}

func tokenCacheConfigChanged(before, after *config.Config) bool {
	if before == nil || after == nil {
		return true
	}
	return before.CacheTokenCount != after.CacheTokenCount ||
		before.CacheTTL != after.CacheTTL ||
		before.CacheStrategy != after.CacheStrategy ||
		before.EnableTokenCache != after.EnableTokenCache ||
		before.TokenCacheTTL != after.TokenCacheTTL ||
		before.TokenCacheStrategy != after.TokenCacheStrategy
}

func (a *API) clearTokenCaches(ctx context.Context) {
	if a.tokenCache != nil {
		if err := a.tokenCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear token cache after config update", "error", err)
		}
	}
	if a.promptCache != nil {
		if err := a.promptCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear prompt cache after config update", "error", err)
		}
	}
}

func configPayload(cfg *config.Config) (map[string]interface{}, error) {
	if cfg == nil {
		return map[string]interface{}{}, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if v, ok := payload["admin_pass"]; ok {
		payload["admin_password"] = v
	}
	return payload, nil
}

func buildConfigFromPatch(r *http.Request, current *config.Config) (*config.Config, error) {
	base := &config.Config{}
	if current != nil {
		copyCfg := *current
		base = &copyCfg
	}

	baseMap, err := configPayload(base)
	if err != nil {
		return nil, err
	}

	patch := map[string]interface{}{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return nil, err
	}

	if v, ok := patch["admin_password"]; ok {
		patch["admin_pass"] = v
	}

	for key, value := range patch {
		baseMap[key] = normalizeConfigPatchValue(key, value)
	}

	raw, err := json.Marshal(baseMap)
	if err != nil {
		return nil, err
	}
	var newCfg config.Config
	if err := json.Unmarshal(raw, &newCfg); err != nil {
		return nil, err
	}
	return &newCfg, nil
}

func normalizeConfigPatchValue(key string, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	switch key {
	case "enable_token_refresh", "enable_usage_refresh", "enable_token_count", "cache_token_count",
		"enable_token_cache", "auto_refresh_token", "kiro_use_builtin_proxy", "warp_use_builtin_proxy",
		"orchids_use_builtin_proxy", "antigravity_use_builtin_proxy", "warp_credit_refund",
		"enable_context_compress", "debug_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "retry_delay", "request_timeout", "refresh_interval", "cache_ttl", "token_cache_ttl",
		"redis_db", "token_refresh_interval", "load_balancer_cache_ttl", "concurrency_limit",
		"concurrency_timeout", "max_retries", "credential_retries":
		if i, ok := parseIntish(value); ok {
			return i
		}
	case "proxy_bypass":
		return normalizeProxyBypassValue(value)
	}

	return value
}

func parseBoolish(value interface{}) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		switch s {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func parseIntish(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func normalizeProxyBypassValue(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		lines := strings.FieldsFunc(v, func(r rune) bool {
			return r == '\n' || r == ','
		})
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func shouldSyncAccountOnCreate(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(acc.AccountType), "orchids")
}

func applyAccountStatusFromError(acc *store.Account, err error) {
	if acc == nil || err == nil {
		return
	}
	status := classifyAccountStatusFromError(err.Error())
	if status == "" {
		return
	}
	acc.StatusCode = status
	acc.LastAttempt = time.Now()
}

func (a *API) persistConfig(ctx context.Context, current, newCfg *config.Config) error {
	if newCfg == nil {
		return fmt.Errorf("config is nil")
	}
	if a.store == nil {
		return fmt.Errorf("settings store not configured")
	}

	config.ApplyHardcoded(newCfg)

	data, err := json.Marshal(newCfg)
	if err != nil {
		return err
	}
	a.config.Store(newCfg)
	if err := a.store.SetSetting(ctx, "config", string(data)); err != nil {
		return err
	}
	if tokenCacheConfigChanged(current, newCfg) {
		a.clearTokenCaches(ctx)
	}
	return nil
}
