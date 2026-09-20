package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

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

// newWorkBuddyLoginClientMu protects the replaceable client factory used by
// tests. Login polling runs in a background goroutine, so a test may restore
// the factory while that goroutine is still winding down after cancellation.
var newWorkBuddyLoginClientMu sync.RWMutex

func workBuddyLoginClientFactory() func(*store.Account, *config.Config) *workbuddy.Client {
	newWorkBuddyLoginClientMu.RLock()
	factory := newWorkBuddyLoginClient
	newWorkBuddyLoginClientMu.RUnlock()
	return factory
}

type workbuddyLogin struct {
	deviceLogin
	factory func(*store.Account, *config.Config) *workbuddy.Client
}

// HandleWorkBuddyLogin starts and observes the official WorkBuddy
// (www.workbuddy.ai) browser authorization flow. The server never sees the
// password: it only opens the official login page and polls for the resulting
// token pair. This handler is registered behind the administrator session
// middleware.
func (a *API) HandleWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	routeBrowserLogin(w, r, "/api/workbuddy/login", a.startWorkBuddyLogin, a.getWorkBuddyLogin, a.cancelWorkBuddyLogin)
}

func (a *API) startWorkBuddyLogin(w http.ResponseWriter, r *http.Request) {
	enabled, ok := beginBrowserLogin(w, r, a, "WorkBuddy", a.workbuddyLogins)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := a.config.Load()
	factory := workBuddyLoginClientFactory()
	client := factory(nil, cfg)
	defer client.Close()
	state, authURL, err := client.StartAuthLogin(ctx, workbuddyClientVersion)
	if err != nil {
		writeAuthorizationStartFailure(w, err, authorizationFailure{
			channel:           "WorkBuddy",
			unreachableDetail: "the server cannot reach www.workbuddy.ai",
			unavailable:       workbuddy.ErrAuthUnavailable,
			rejected:          workbuddy.ErrAuthRejected,
		})
		return
	}
	slog.Debug("WorkBuddy authorization started", "login_state_host", "www.workbuddy.ai")

	admitBrowserLogin(w, a.workbuddyLogins, "WorkBuddy", loginSeed{
		deviceCode: state,
		verifyFull: authURL,
		expiresAt:  time.Now().Add(workbuddyLoginTTL),
		interval:   workbuddyLoginInterval,
		message:    "Waiting for WorkBuddy authorization",
		enabled:    enabled,
		cfg:        cfg,
	}, func(shared deviceLogin) *workbuddyLogin {
		return &workbuddyLogin{deviceLogin: shared, factory: factory}
	}, a.pollWorkBuddyLogin)
}

func (a *API) getWorkBuddyLogin(w http.ResponseWriter, id string) {
	respondLoginStatus(w, a.workbuddyLogins, id, "WorkBuddy")
}

func (a *API) cancelWorkBuddyLogin(w http.ResponseWriter, id string) {
	abandonLogin(w, a.workbuddyLogins, id, "WorkBuddy")
}

// pollWorkBuddyLogin exchanges the authorization state until the browser step
// completes, then verifies and persists the account.
func (a *API) pollWorkBuddyLogin(ctx context.Context, id string) {
	login, ok := a.workbuddyLogins.pollable(id)
	if !ok {
		return
	}
	client := login.factory(nil, login.configSnapshot)
	defer client.Close()

	for {
		login, ok := awaitLoginPoll(ctx, a.workbuddyLogins, id, "WorkBuddy")
		if !ok {
			return
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
			slog.Warn("WorkBuddy authorization failed", "login_id", id, "error", err)
			a.workbuddyLogins.finish(id, "failed", "WorkBuddy authorization failed; start again", 0)
			return
		}

		account, err := a.buildWorkBuddyAccountFromCredentialsWithFactory(ctx, id, creds, login.configSnapshot, login.factory)
		if err != nil {
			slog.Warn("WorkBuddy authorization succeeded but verification failed", "login_id", id, "error", err)
			a.workbuddyLogins.finish(id, "failed", "WorkBuddy authorization succeeded but the account could not be verified", 0)
			return
		}
		if ctx.Err() != nil || !a.workbuddyLogins.pending(id) {
			return
		}
		if login.enabledKnown {
			account.Enabled = login.enabled
		}
		finishBrowserLogin(a, ctx, a.workbuddyLogins, id, "WorkBuddy", account, func(acc *store.Account) {
			acc.ReplaceWorkBuddyCredentials = true
		})
		return
	}
}

// buildWorkBuddyAccountFromCredentials turns a completed login into a verified
// account record. The model catalog read doubles as the credential check: a
// token that cannot list the account catalog is never persisted.
func (a *API) buildWorkBuddyAccountFromCredentialsWithFactory(ctx context.Context, loginID string, creds workbuddy.Credentials, cfg *config.Config, factory func(*store.Account, *config.Config) *workbuddy.Client) (*store.Account, error) {
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

	state := a.workbuddyLogins.deviceCode(loginID)
	client := factory(acc, cfg)
	defer client.Close()

	// The access-token JWT already proves the identity; the profile endpoint only
	// adds the display nickname, so its failure is not fatal.
	resolved := workbuddy.ResolveCredentials(acc)
	if uid, nickname, email, err := client.FetchAccountIdentity(ctx, acc.WorkBuddyAccessToken, state); err == nil {
		acc.Name = strings.TrimSpace(nickname)
		if acc.WorkBuddyUID == "" {
			acc.WorkBuddyUID = uid
		}
		if resolved.Email == "" && email != "" {
			acc.Email = email
		}
	}
	if acc.WorkBuddyUID == "" {
		acc.WorkBuddyUID = resolved.UID
	}
	if strings.TrimSpace(acc.Email) == "" {
		acc.Email = resolved.Email
	}
	if strings.TrimSpace(acc.Name) == "" {
		switch {
		case strings.TrimSpace(acc.Email) != "":
			acc.Name = acc.Email
		case len(acc.WorkBuddyUID) >= 8:
			acc.Name = "workbuddy-" + acc.WorkBuddyUID[:8]
		default:
			acc.Name = "workbuddy-login"
		}
	}

	models, err := client.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := workbuddy.CatalogSnapshot(models)
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
