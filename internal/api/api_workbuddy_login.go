package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

const (
	// workbuddyLoginTTL is how long an authorization transaction stays alive.
	// The official desktop client waits 300s; the console keeps a longer window
	// because the operator may have to sign in to Keycloak first.
	workbuddyLoginTTL = 15 * time.Minute
	// workbuddyLoginInterval is the upstream poll cadence (the client uses 1s).
	workbuddyLoginInterval = 2 * time.Second
	// workbuddyClientVersion is appended to the login URL, as the desktop
	// client does.
	workbuddyClientVersion = "5.5.2"
)

// newWorkBuddyLoginClient builds the client used by the login transaction. It
// is a variable so tests can drive the flow without touching the network.
var newWorkBuddyLoginClient = func(acc *store.Account, cfg *config.Config) *workbuddy.Client {
	return workbuddy.NewFromAccount(acc, cfg)
}

// HandleWorkBuddyLogin starts and observes the official WorkBuddy
// (www.workbuddy.ai) browser authorization flow. The server never sees the
// password: it only opens the official login page and polls for the resulting
// token pair. This handler is registered behind the administrator session
// middleware.
func (a *API) HandleWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workbuddy/login"), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		a.startWorkBuddyLogin(w, r)
	case r.Method == http.MethodGet && path != "":
		w.Header().Set("Content-Type", "application/json")
		a.getWorkBuddyLogin(w, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelWorkBuddyLogin(w, path)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		http.Error(w, "account store is not configured", http.StatusServiceUnavailable)
		return
	}
	if !sameOriginAdminRequest(w, r, "WorkBuddy") {
		return
	}
	options, ok := parseWorkBuddyLoginOptions(w, r)
	if !ok {
		return
	}
	enabled := true
	if options.Enabled != nil {
		enabled = *options.Enabled
	}

	a.cleanupWorkBuddyLogins(time.Now())

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	client := newWorkBuddyLoginClient(nil, a.config.Load())
	defer client.Close()
	state, authURL, err := client.StartAuthLogin(ctx, workbuddyClientVersion)
	if err != nil {
		// Upstream failures can echo request material; never relay them.
		http.Error(w, "failed to start WorkBuddy authorization", http.StatusBadGateway)
		return
	}

	id, err := newDeviceLoginID()
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &workbuddyLogin{
		deviceCode: state,
		verifyFull: authURL,
		expiresAt:  time.Now().Add(workbuddyLoginTTL),
		interval:   workbuddyLoginInterval,
		cancel:     pollCancel,
		status:     "pending",
		message:    "Waiting for WorkBuddy authorization",
		enabled:    enabled,
		// The caller always declares the intended state, so the completed
		// account must honour it instead of defaulting to enabled.
		enabledKnown: true,
	}

	a.workbuddyLoginMu.Lock()
	if len(a.workbuddyLogins) >= maxDeviceLogins {
		a.workbuddyLoginMu.Unlock()
		pollCancel()
		http.Error(w, "too many pending WorkBuddy logins", http.StatusTooManyRequests)
		return
	}
	a.workbuddyLogins[id] = login
	a.workbuddyLoginMu.Unlock()

	go a.pollWorkBuddyLogin(pollContext, id)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(newDeviceLoginResponse(id, login))
}

func (a *API) getWorkBuddyLogin(w http.ResponseWriter, id string) {
	a.cleanupWorkBuddyLogins(time.Now())
	a.workbuddyLoginMu.Lock()
	login := a.workbuddyLogins[id]
	response := newDeviceLoginResponse(id, login)
	a.workbuddyLoginMu.Unlock()
	if login == nil {
		http.Error(w, "WorkBuddy login not found", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (a *API) cancelWorkBuddyLogin(w http.ResponseWriter, id string) {
	a.workbuddyLoginMu.Lock()
	login := a.workbuddyLogins[id]
	if login == nil {
		a.workbuddyLoginMu.Unlock()
		http.Error(w, "WorkBuddy login not found", http.StatusNotFound)
		return
	}
	delete(a.workbuddyLogins, id)
	finishDeviceLogin(login, "cancelled", "WorkBuddy authorization cancelled", 0)
	a.workbuddyLoginMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// pollWorkBuddyLogin exchanges the authorization state until the browser step
// completes, then verifies and persists the account.
func (a *API) pollWorkBuddyLogin(ctx context.Context, id string) {
	client := newWorkBuddyLoginClient(nil, a.config.Load())
	defer client.Close()

	for {
		login, ok := a.workBuddyLoginForPoll(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.finishWorkBuddyLogin(id, "expired", "WorkBuddy authorization timed out; start again", 0)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
		}

		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		creds, err := client.PollAuthLogin(reqCtx, login.deviceCode)
		cancel()
		if err != nil {
			if errors.Is(err, workbuddy.ErrAuthPending) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// Authorization itself failed (state consumed, cancelled, or the
			// upstream rejected it). Report it without echoing upstream text.
			a.finishWorkBuddyLogin(id, "failed", "WorkBuddy authorization failed; start again", 0)
			return
		}

		account, err := a.buildWorkBuddyAccountFromCredentials(ctx, id, creds)
		if err != nil {
			a.finishWorkBuddyLogin(id, "failed", "WorkBuddy authorization succeeded but the account could not be verified", 0)
			return
		}
		if login.enabledKnown {
			account.Enabled = login.enabled
		}
		existing, err := a.findDuplicateAccountByCredential(ctx, account, 0)
		if err != nil {
			a.finishWorkBuddyLogin(id, "failed", "WorkBuddy authorization succeeded but the account could not be saved", 0)
			return
		}
		if existing != nil {
			account.ID = existing.ID
			if err := a.store.UpdateAccount(ctx, account); err != nil {
				a.finishWorkBuddyLogin(id, "failed", "WorkBuddy authorization succeeded but the account could not be updated", 0)
				return
			}
			a.finishWorkBuddyLogin(id, "complete", "WorkBuddy account credentials refreshed", account.ID)
			return
		}
		if err := a.store.CreateAccount(ctx, account); err != nil {
			a.finishWorkBuddyLogin(id, "failed", "WorkBuddy authorization succeeded but the account could not be saved", 0)
			return
		}
		a.finishWorkBuddyLogin(id, "complete", "WorkBuddy account added", account.ID)
		a.syncAccountAfterCreate(*account)
		return
	}
}

// buildWorkBuddyAccountFromCredentials turns a completed login into a verified
// account record. The model catalog read doubles as the credential check: a
// token that cannot list the account catalog is never persisted.
func (a *API) buildWorkBuddyAccountFromCredentials(ctx context.Context, loginID string, creds workbuddy.Credentials) (*store.Account, error) {
	acc := &store.Account{
		Name:                  "workbuddy-login",
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  strings.TrimSpace(creds.AccessToken),
		WorkBuddyRefreshToken: strings.TrimSpace(creds.RefreshToken),
		WorkBuddyUID:          strings.TrimSpace(creds.UID),
		Email:                 strings.TrimSpace(creds.Email),
		WorkBuddyExpiresAt:    creds.ExpiresAt,
		Weight:                1,
		Enabled:               true,
	}
	if acc.WorkBuddyExpiresAt.IsZero() && !creds.ExpiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = creds.ExpiresAt
	}

	state := a.workBuddyLoginState(loginID)
	client := newWorkBuddyLoginClient(acc, a.config.Load())
	defer client.Close()

	if uid, nickname, email, err := client.FetchAccountIdentity(ctx, acc.WorkBuddyAccessToken, state); err == nil {
		if uid != "" {
			acc.WorkBuddyUID = uid
		}
		if nickname != "" {
			acc.Name = nickname
		}
		if email != "" {
			acc.Email = email
		}
	}
	if acc.Name == "" || acc.Name == "workbuddy-login" {
		if acc.Email != "" {
			acc.Name = acc.Email
		} else if len(acc.WorkBuddyUID) >= 8 {
			acc.Name = "workbuddy-" + acc.WorkBuddyUID[:8]
		}
	}

	models, err := client.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("workbuddy catalog was empty")
	}
	acc.WorkBuddyModelIDs = ids
	acc.WorkBuddyModelsSyncedAt = time.Now()

	if strings.TrimSpace(acc.WorkBuddyRefreshToken) == "" {
		// Without a refresh token the account cannot survive its first token
		// expiry; refresh immediately so the durable credential is captured.
		refreshed, refreshErr := client.RefreshCredentials(ctx)
		if refreshErr != nil || strings.TrimSpace(refreshed.RefreshToken) == "" {
			return nil, errors.New("workbuddy login returned no refresh token")
		}
		acc.WorkBuddyAccessToken = refreshed.AccessToken
		acc.WorkBuddyRefreshToken = refreshed.RefreshToken
		if !refreshed.ExpiresAt.IsZero() {
			acc.WorkBuddyExpiresAt = refreshed.ExpiresAt
		}
		if refreshed.UID != "" {
			acc.WorkBuddyUID = refreshed.UID
		}
	}
	if !NormalizeWorkBuddyCredentials(acc) {
		return nil, errWorkBuddyMissingCredential
	}
	return acc, nil
}

func (a *API) workBuddyLoginForPoll(id string) (*workbuddyLogin, bool) {
	a.workbuddyLoginMu.Lock()
	defer a.workbuddyLoginMu.Unlock()
	return deviceLoginForPoll(a.workbuddyLogins, id)
}

func (a *API) workBuddyLoginState(id string) string {
	a.workbuddyLoginMu.Lock()
	defer a.workbuddyLoginMu.Unlock()
	if login := a.workbuddyLogins[id]; login != nil {
		return login.deviceCode
	}
	return ""
}

func (a *API) finishWorkBuddyLogin(id, status, message string, accountID int64) {
	a.workbuddyLoginMu.Lock()
	defer a.workbuddyLoginMu.Unlock()
	finishDeviceLogin(a.workbuddyLogins[id], status, message, accountID)
}

func (a *API) cleanupWorkBuddyLogins(now time.Time) {
	if a == nil {
		return
	}
	a.workbuddyLoginMu.Lock()
	defer a.workbuddyLoginMu.Unlock()
	cleanupDeviceLogins(a.workbuddyLogins, now, "WorkBuddy authorization timed out; start again")
}

// sameOriginAdminRequest enforces that a credential-mutating login attempt came
// from this admin origin over a secure context. It writes the rejection itself.
// Any request body is the caller's concern; this helper never reads it.
func sameOriginAdminRequest(w http.ResponseWriter, r *http.Request, provider string) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
		!strings.EqualFold(origin.Host, r.Host) || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" ||
		r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "same-origin browser login required", http.StatusForbidden)
		return false
	}
	if origin.Scheme != "https" && origin.Hostname() != "localhost" && origin.Hostname() != "127.0.0.1" && origin.Hostname() != "::1" {
		http.Error(w, "HTTPS required for "+provider+" login", http.StatusForbidden)
		return false
	}
	// A body is optional for the popup flow, but reject a type we do not parse
	// instead of silently ignoring whatever was sent.
	if rawType := strings.TrimSpace(r.Header.Get("Content-Type")); rawType != "" {
		if mediaType, _, _ := mime.ParseMediaType(rawType); mediaType != "application/json" {
			http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
			return false
		}
	}
	return true
}

// workBuddyLoginOptions parses the optional start body.
type workBuddyLoginOptions struct {
	Enabled *bool `json:"enabled"`
}

func parseWorkBuddyLoginOptions(w http.ResponseWriter, r *http.Request) (workBuddyLoginOptions, bool) {
	var options workBuddyLoginOptions
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return options, false
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed == "" {
		return options, true
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return workBuddyLoginOptions{}, false
	}
	return options, true
}
