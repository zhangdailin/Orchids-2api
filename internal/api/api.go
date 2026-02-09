package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/warp"
)

type API struct {
	store        *store.Store
	summaryCache prompt.SummaryCache
	tokenCache   tokencache.Cache
	adminUser    string
	adminPass    string
	configMu     sync.RWMutex
	config       interface{} // Using interface{} to avoid circular dependency if any, or just use *config.Config
	configPath   string      // Path to config.json
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

func New(s *store.Store, adminUser, adminPass string, cfg interface{}, cfgPath string) *API {
	return &API{
		store:      s,
		adminUser:  adminUser,
		adminPass:  adminPass,
		config:     cfg,
		configPath: cfgPath,
	}
}

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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

	if req.Username != a.adminUser || req.Password != a.adminPass {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
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
		a.configMu.RLock()
		json.NewEncoder(w).Encode(a.config)
		a.configMu.RUnlock()
	case http.MethodPost:
		// Update config under write lock
		a.configMu.Lock()
		if err := json.NewDecoder(r.Body).Decode(a.config); err != nil {
			a.configMu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Save to Redis
		data, err := json.Marshal(a.config)
		if err != nil {
			a.configMu.Unlock()
			http.Error(w, "Failed to marshal config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		a.configMu.Unlock()

		if err := a.store.SetSetting(r.Context(), "config", string(data)); err != nil {
			http.Error(w, "Failed to save config to Redis: "+err.Error(), http.StatusInternalServerError)
			return
		}

		a.configMu.RLock()
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(a.config)
		a.configMu.RUnlock()
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
			normalized = append(normalized, normalizeWarpTokenOutput(acc))
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
		if acc.ClientCookie != "" && acc.SessionID == "" && !strings.EqualFold(acc.AccountType, "warp") {
			info, err := clerk.FetchAccountInfo(acc.ClientCookie)
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
		json.NewEncoder(w).Encode(normalizeWarpTokenOutput(&acc))

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
	isUsage := len(parts) > 1 && parts[1] == "usage"

	switch r.Method {
	case http.MethodGet:
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
				"usage_daily":    acc.UsageDaily,
				"usage_total":    acc.UsageTotal,
				"quota_reset_at": acc.QuotaResetAt,
				"reset_date":     acc.ResetDate,
			})
			return
		}
		if isRefresh {
			acc, err := a.store.GetAccount(r.Context(), id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			if strings.EqualFold(acc.AccountType, "warp") {
				var cfg *config.Config
				a.configMu.RLock()
				if raw, ok := a.config.(*config.Config); ok {
					cfg = raw
				}
				a.configMu.RUnlock()
				warpClient := warp.NewFromAccount(acc, cfg)
				jwt, err := warpClient.RefreshAccount(r.Context())
				if err != nil {
					status := http.StatusBadRequest
					if code := warp.HTTPStatusCode(err); code >= 400 {
						status = code
					}
					http.Error(w, "Failed to refresh warp account: "+err.Error(), status)
					return
				}
				acc.Token = jwt
				warpClient.SyncAccountState()
			} else {
				info, err := clerk.FetchAccountInfo(acc.ClientCookie)
				if err != nil {
					http.Error(w, "Failed to refresh account: "+err.Error(), http.StatusBadRequest)
					return
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
				}
			}

			// 刷新成功后清理账号状态
			acc.StatusCode = ""
			acc.LastAttempt = time.Time{}
			acc.QuotaResetAt = time.Time{}

			if err := a.store.UpdateAccount(r.Context(), acc); err != nil {
				http.Error(w, "Failed to save refreshed account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(normalizeWarpTokenOutput(acc))
			return
		}
		acc, err := a.store.GetAccount(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(normalizeWarpTokenOutput(acc))

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

		if err := a.store.UpdateAccount(r.Context(), &acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(normalizeWarpTokenOutput(&acc))

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
		exportData.Accounts[i] = *normalizeWarpTokenOutput(acc)
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
		} else if acc.ClientCookie != "" {
			clientJWT, sessionJWT, err := clerk.ParseClientCookies(acc.ClientCookie)
			if err != nil {
				slog.Warn("Invalid client cookie in import", "name", acc.Name, "error", err)
				result.Skipped++
				continue
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

func (a *API) SetSummaryCache(c prompt.SummaryCache) {
	a.summaryCache = c
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
	a.configMu.RLock()
	cfg, ok := a.config.(*config.Config)
	a.configMu.RUnlock()
	if !ok || cfg == nil {
		return false
	}
	return cfg.CacheTokenCount
}
