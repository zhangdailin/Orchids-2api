package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/middleware"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

type API struct {
	store        *store.Store
	tokenCache   tokencache.Cache
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func normalizeWarpTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "warp") {
		return
	}
	if acc.RefreshToken == "" && acc.ClientCookie != "" {
		acc.RefreshToken = acc.ClientCookie
	}
	// Warp 只使用 refresh_token，清理 client_cookie 避免混用
	acc.ClientCookie = ""
	acc.SessionCookie = ""
}

func normalizeWarpTokenOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	copyAcc := *acc
	if strings.EqualFold(copyAcc.AccountType, "warp") {
		if copyAcc.RefreshToken == "" && copyAcc.ClientCookie != "" {
			copyAcc.RefreshToken = copyAcc.ClientCookie
		}
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
		config.ApplyHardcoded(&newCfg)

		// Save to Redis
		data, err := json.Marshal(&newCfg)
		if err != nil {
			http.Error(w, "Failed to marshal config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		a.config.Store(&newCfg)

		if err := a.store.SetSetting(r.Context(), "config", string(data)); err != nil {
			http.Error(w, "Failed to save config to Redis: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(&newCfg)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
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
			acc.ClientCookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(acc.ClientCookie), "Bearer "))
			clientJWT, sessionJWT, err := clerk.ParseClientCookies(acc.ClientCookie)
			if err != nil {
				// Orchids: users often paste something that *looks* like a JWT.
				// But there are two different JWT-like things:
				//   1) Clerk "__client" cookie JWT (may contain rotating_token) -> should be stored as cookie
				//   2) Upstream bearer token JWT (clerk session token) -> can be stored as acc.Token
				if isLikelyJWT(acc.ClientCookie) {
					if jwtHasRotatingToken(acc.ClientCookie) {
						// Treat as __client cookie value, not bearer token.
						acc.SessionCookie = ""
						acc.SessionID = ""
						acc.Token = ""
					} else {
						acc.Token = strings.TrimSpace(acc.ClientCookie)
						acc.ClientCookie = ""
						acc.SessionCookie = ""
						acc.SessionID = ""
					}
				} else {
					http.Error(w, "Invalid client cookie: "+err.Error(), http.StatusBadRequest)
					return
				}
			} else {
				acc.ClientCookie = clientJWT
				if sessionJWT != "" {
					acc.SessionCookie = sessionJWT
					if acc.SessionID == "" {
						if sid, sub := clerk.ParseSessionInfoFromJWT(sessionJWT); sid != "" {
							acc.SessionID = sid
							if acc.UserID == "" {
								acc.UserID = sub
							}
						}
					}
				}
			}
		}
		if acc.ClientCookie != "" && acc.SessionID == "" && !strings.EqualFold(acc.AccountType, "warp") {
			cfg := a.config.Load()
			proxyFunc := http.ProxyFromEnvironment
			if cfg != nil {
				proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
			}
			info, err := clerk.FetchAccountInfoWithSessionProxy(acc.ClientCookie, acc.SessionCookie, proxyFunc)
			if err != nil {
				slog.Warn("Failed to fetch account info, saving without session data", "error", err)
			} else {
				acc.SessionID = info.SessionID
				acc.ClientUat = info.ClientUat
				acc.ProjectID = info.ProjectID
				acc.UserID = info.UserID
				acc.Email = info.Email
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"account_id":     acc.ID,
				"name":           acc.Name,
				"account_type":   acc.AccountType,
				"subscription":   acc.Subscription,
				"usage_current":  acc.UsageCurrent,
				"usage_limit":    acc.UsageLimit,
				"usage_total":    acc.UsageTotal,
				"quota_reset_at": acc.QuotaResetAt,
			})
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
				d := time.Duration(1<<minInt(fails, 8)) * time.Second
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

			if strings.EqualFold(acc.AccountType, "warp") {
				cfg := a.config.Load()
				warpClient := warp.NewFromAccount(acc, cfg)
				jwt, err := warpClient.RefreshAccount(r.Context())
				if err != nil {
					status := http.StatusBadRequest
					if code := warp.HTTPStatusCode(err); code >= 400 {
						status = code
					}
					if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
						acc.StatusCode = strconv.Itoa(status)
						acc.LastAttempt = time.Now()
						if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
							slog.Warn("Failed to persist warp refresh status", "account_id", acc.ID, "error", updateErr)
						}
					}
					http.Error(w, "Failed to refresh warp account: "+err.Error(), status)
					return
				}
				acc.Token = jwt
				warpClient.SyncAccountState()

				limitCtx, limitCancel := context.WithTimeout(r.Context(), 15*time.Second)
				limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
				limitCancel()
				if limitErr == nil && limitInfo != nil {
					if limitInfo.IsUnlimited {
						acc.Subscription = "unlimited"
					} else {
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
				}
			} else if strings.EqualFold(acc.AccountType, "grok") {
				if strings.TrimSpace(acc.ClientCookie) == "" {
					http.Error(w, "Failed to verify grok account: missing sso token", http.StatusBadRequest)
					return
				}

				cfg := a.config.Load()
				client := grok.New(cfg)

				verifyCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
				info, verifyErr := client.VerifyToken(verifyCtx, acc.ClientCookie, acc.AgentMode)
				cancel()
				if verifyErr != nil {
					status := classifyAccountStatusFromError(verifyErr.Error())
					if status != "" {
						checkErrStatus = status
						acc.StatusCode = status
						acc.LastAttempt = time.Now()
						if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
							slog.Warn("Failed to persist grok verify status", "account_id", acc.ID, "error", updateErr)
						}
					}
					http.Error(w, "Failed to verify grok account: "+verifyErr.Error(), httpStatusFromAccountStatus(status))
					return
				}

				if info != nil {
					limit := info.Limit
					remaining := info.Remaining
					if remaining < 0 {
						remaining = 0
					}
					if limit <= 0 && remaining > 0 {
						limit = remaining
					}
					if limit > 0 {
						acc.UsageLimit = float64(limit)
						acc.UsageCurrent = float64(remaining)
					}
					if !info.ResetAt.IsZero() {
						acc.QuotaResetAt = info.ResetAt
					}
				}
			} else {
				cfg := a.config.Load()
				proxyFunc := http.ProxyFromEnvironment
				if cfg != nil {
					proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
				}
				info, err := clerk.FetchAccountInfoWithSessionProxy(acc.ClientCookie, acc.SessionCookie, proxyFunc)
				if err != nil {
					refreshErr := err
					// Fallback: when Clerk cannot enumerate active sessions, try session-id token endpoint.
					if strings.Contains(strings.ToLower(err.Error()), "no active sessions found") && strings.TrimSpace(acc.SessionID) != "" {
						orchidsClient := orchids.NewFromAccount(acc, cfg)
						jwt, jwtErr := orchidsClient.GetToken()
						if jwtErr == nil && strings.TrimSpace(jwt) != "" {
							acc.Token = jwt
							refreshErr = nil
							slog.Warn("Orchids refresh: no active sessions, fallback token refresh succeeded", "account_id", id)
						} else if jwtErr != nil {
							refreshErr = errors.New(err.Error() + "; fallback token error: " + jwtErr.Error())
						}
					}

					if refreshErr != nil {
						status := classifyAccountStatusFromError(refreshErr.Error())
						if status != "" {
							acc.StatusCode = status
							acc.LastAttempt = time.Now()
							if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
								slog.Warn("Failed to persist orchids refresh status", "account_id", acc.ID, "error", updateErr)
							}
						}
						http.Error(w, "Failed to refresh account: "+refreshErr.Error(), http.StatusBadRequest)
						return
					}
				} else {
					slog.Info("Orchids refresh: clerk info", "account_id", id, "has_jwt", info.JWT != "", "email", info.Email)
					acc.SessionID = info.SessionID
					acc.ClientUat = info.ClientUat
					acc.ProjectID = info.ProjectID
					acc.UserID = info.UserID
					acc.Email = info.Email
					acc.Token = info.JWT // Update Token/JWT
					if info.ClientCookie != "" {
						acc.ClientCookie = info.ClientCookie
					}

					// Sync Orchids credits
					if info.JWT != "" {
						uid := info.UserID
						if strings.TrimSpace(uid) == "" {
							uid = acc.UserID
						}
						creditsInfo, creditsErr := orchids.FetchCreditsWithProxy(r.Context(), info.JWT, uid, proxyFunc)
						if creditsErr != nil {
							slog.Warn("Orchids credits sync failed on refresh", "account", acc.Name, "error", creditsErr)
						} else if creditsInfo != nil {
							acc.Subscription = strings.ToLower(creditsInfo.Plan)
							acc.UsageCurrent = creditsInfo.Credits
							acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
						}
					}
				}
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
			clientJWT, sessionJWT, err := clerk.ParseClientCookies(acc.ClientCookie)
			if err != nil {
				http.Error(w, "Invalid client cookie: "+err.Error(), http.StatusBadRequest)
				return
			}
			acc.ClientCookie = clientJWT
			if sessionJWT != "" {
				acc.SessionCookie = sessionJWT
				if acc.SessionID == "" {
					if sid, sub := clerk.ParseSessionInfoFromJWT(sessionJWT); sid != "" {
						acc.SessionID = sid
						if acc.UserID == "" {
							acc.UserID = sub
						}
					}
				}
			}
		}

		if acc.SessionID == "" {
			acc.SessionID = existing.SessionID
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
			acc.ClientCookie = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(acc.ClientCookie), "Bearer "))
			clientJWT, sessionJWT, err := clerk.ParseClientCookies(acc.ClientCookie)
			if err != nil {
				if isLikelyJWT(acc.ClientCookie) {
					if jwtHasRotatingToken(acc.ClientCookie) {
						acc.SessionCookie = ""
						acc.SessionID = ""
						acc.Token = ""
					} else {
						acc.Token = strings.TrimSpace(acc.ClientCookie)
						acc.ClientCookie = ""
						acc.SessionCookie = ""
						acc.SessionID = ""
					}
				} else {
					slog.Warn("Invalid client cookie in import", "name", acc.Name, "error", err)
					result.Skipped++
					continue
				}
			} else {
				acc.ClientCookie = clientJWT
				if sessionJWT != "" {
					acc.SessionCookie = sessionJWT
					if acc.SessionID == "" {
						if sid, sub := clerk.ParseSessionInfoFromJWT(sessionJWT); sid != "" {
							acc.SessionID = sid
							if acc.UserID == "" {
								acc.UserID = sub
							}
						}
					}
				}
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
