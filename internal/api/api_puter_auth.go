package api

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
)

var puterFetchUser = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.User, error) {
	return puter.NewFromAccount(acc, cfg).FetchUser(ctx)
}

// HandlePuterWebLogin accepts a popup grant, not credentials or identity from
// the login form. It is registered behind the administrator session middleware.
func (a *API) HandlePuterWebLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Bind the submission to this admin origin. Do not trust forwarded hosts.
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
		!strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
		r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "same-origin browser login required", http.StatusForbidden)
		return
	}
	if origin.Scheme != "https" && origin.Hostname() != "localhost" && origin.Hostname() != "127.0.0.1" && origin.Hostname() != "::1" {
		http.Error(w, "HTTPS required for Puter login", http.StatusForbidden)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	var input struct {
		Token   string `json:"token"`
		Enabled bool   `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return
	}
	input.Token = strings.TrimSpace(input.Token)
	if input.Token == "" || len(input.Token) > 16<<10 || strings.ContainsAny(input.Token, "\r\n\t ") {
		http.Error(w, "invalid Puter grant", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	acc := &store.Account{AccountType: "puter", ClientCookie: input.Token, Weight: 1, Enabled: input.Enabled}
	user, err := puterFetchUser(ctx, acc, a.config.Load())
	if err != nil || user == nil {
		// Raw upstream errors can contain credentials. Never echo them.
		http.Error(w, "Puter could not verify the login grant; please retry official login", http.StatusBadGateway)
		return
	}
	acc.Name, acc.UserID = user.Username, user.UUID
	usage, err := puterFetchMonthlyUsage(ctx, acc, a.config.Load())
	if err != nil || usage == nil {
		http.Error(w, "Puter login succeeded but the usage API could not be verified; account was not saved", http.StatusBadGateway)
		return
	}
	applyPuterMonthlyUsage(acc, usage)
	if acc.UsageLimit > 0 && acc.UsageCurrent <= 0 {
		acc.StatusCode = "402"
	}
	existing, err := a.findDuplicateAccountByCredential(ctx, acc, 0)
	if err != nil {
		http.Error(w, "could not check existing Puter accounts", http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if existing != nil {
		acc.ID = existing.ID
		status = http.StatusOK
	} else if err := a.store.CreateAccount(ctx, acc); err != nil {
		http.Error(w, "could not save Puter login", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"account_id": acc.ID, "status": "complete"})
}
