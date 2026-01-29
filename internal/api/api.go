package api

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/clerk"
	"orchids-api/internal/model"
	"orchids-api/internal/store"
)

type API struct {
	store      *store.Store
	adminUser  string
	adminPass  string
	config     interface{} // Using interface{} to avoid circular dependency if any, or just use *config.Config
	configPath string      // Path to config.json
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

	// Set session cookie
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(a.adminPass)))
	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 7, // 1 week
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
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

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/accounts", a.HandleAccounts)
	mux.HandleFunc("/api/accounts/", a.HandleAccountByID)
	mux.HandleFunc("/api/keys", a.HandleKeys)
	mux.HandleFunc("/api/keys/", a.HandleKeyByID)
	mux.HandleFunc("/api/models", a.HandleModels)
	mux.HandleFunc("/api/models/", a.HandleModelByID)
	mux.HandleFunc("/api/config", a.HandleConfig)
}

func (a *API) HandleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// We need to type assert since we used interface{} to avoid imports if needed
	// Actually we can just import config if we need to. Let's see.
	// For now let's use json marshal on the interface.

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(a.config)
	case http.MethodPost:
		// Update config
		if err := json.NewDecoder(r.Body).Decode(a.config); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Save to file
		// We'll need to call the Save method. We can use reflection or just import the package.
		// Let's import the package since it's already used in New.

		if saver, ok := a.config.(interface{ Save(string) error }); ok {
			if err := saver.Save(a.configPath); err != nil {
				http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(a.config)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		accounts, err := a.store.ListAccounts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(accounts)

	case http.MethodPost:
		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if acc.ClientCookie != "" && acc.SessionID == "" {
			info, err := clerk.FetchAccountInfo(acc.ClientCookie)
			if err != nil {
				log.Printf("Failed to fetch account info: %v", err)
				http.Error(w, "Failed to fetch account info: "+err.Error(), http.StatusBadRequest)
				return
			}
			acc.SessionID = info.SessionID
			acc.ClientUat = info.ClientUat
			acc.ProjectID = info.ProjectID
			acc.UserID = info.UserID
			acc.Email = info.Email
		}

		if err := a.store.CreateAccount(&acc); err != nil {
			log.Printf("Failed to create account: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(acc)

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

	switch r.Method {
	case http.MethodGet:
		if isRefresh {
			acc, err := a.store.GetAccount(id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			info, err := clerk.FetchAccountInfo(acc.ClientCookie)
			if err != nil {
				http.Error(w, "Failed to refresh account: "+err.Error(), http.StatusBadRequest)
				return
			}
			acc.SessionID = info.SessionID
			acc.ClientUat = info.ClientUat
			acc.ProjectID = info.ProjectID
			acc.UserID = info.UserID
			acc.Email = info.Email
			acc.Token = info.JWT // Update Token/JWT

			if err := a.store.UpdateAccount(acc); err != nil {
				http.Error(w, "Failed to save refreshed account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(acc)
			return
		}
		acc, err := a.store.GetAccount(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(acc)

	case http.MethodPut:
		existing, err := a.store.GetAccount(id)
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

		if acc.SessionID == "" {
			acc.SessionID = existing.SessionID
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

		if err := a.store.UpdateAccount(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(acc)

	case http.MethodDelete:
		if err := a.store.DeleteAccount(id); err != nil {
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

	accounts, err := a.store.ListAccounts()
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
		exportData.Accounts[i] = *acc
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
		if err := a.store.CreateAccount(&acc); err != nil {
			log.Printf("Failed to import account %s: %v", acc.Name, err)
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
		keys, err := a.store.ListApiKeys()
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
			log.Printf("Failed to generate api key: %v", err)
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
		if err := a.store.CreateApiKey(&key); err != nil {
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

		if err := a.store.UpdateApiKeyEnabled(id, *req.Enabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		key, err := a.store.GetApiKeyByID(id)
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
		if err := a.store.DeleteApiKey(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
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
		models, err := a.store.ListModels()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(models)

	case http.MethodPost:
		var m model.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := a.store.CreateModel(&m); err != nil {
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
		m, err := a.store.GetModel(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || err.Error() == "redis: nil" {
				http.Error(w, "Model not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodPut:
		var m model.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.ID = id

		if err := a.store.UpdateModel(&m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodDelete:
		if err := a.store.DeleteModel(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
