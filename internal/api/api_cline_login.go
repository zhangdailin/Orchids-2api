package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// The Cline channel is OAuth-only, and the console drives the whole flow:
//
//	POST   /api/cline/login          start a device authorization transaction
//	GET    /api/cline/login/{id}     observe it while the operator authorizes
//	DELETE /api/cline/login/{id}     abandon it
//
// Only the user code and the official page URL ever reach the browser. The
// device code — the half that can be exchanged for a credential — stays
// server-side, so a compromised browser session cannot mint a credential from a
// half-finished transaction.

const (
	// clineLoginInterval is the poll cadence. WorkOS states its own; this is the
	// ceiling when it does not, and it is deliberately slower than the CLI's
	// single-second poll so a stalled transaction cannot hammer the endpoint for
	// fifteen minutes.
	clineLoginInterval = 2 * time.Second
	// clineLoginTTL bounds one transaction on this side.
	clineLoginTTL = 15 * time.Minute
)

// newClineLoginClient builds the client used by a login transaction. It is a
// variable so tests can drive the flow without touching the network.
var newClineLoginClient = func(acc *store.Account, cfg *config.Config) *cline.Client {
	return cline.NewFromAccount(acc, cfg)
}

var newClineLoginClientMu sync.RWMutex

func clineLoginClientFactory() func(*store.Account, *config.Config) *cline.Client {
	newClineLoginClientMu.RLock()
	factory := newClineLoginClient
	newClineLoginClientMu.RUnlock()
	return factory
}

// clineLoginTransaction is the server-side half of an in-flight login. It is
// held in the shared device-login map so the flow reuses the console's existing
// bookkeeping, but the private material never leaves this file.
//
// The device code rides in the shared deviceLogin because the registry's
// readiness test requires it, and the polling response never serializes that
// field: deviceLoginResponse carries only the user code and the page, so the
// exchangeable half cannot reach the console.
type clineLoginTransaction struct {
	deviceLogin
	factory func(*store.Account, *config.Config) *cline.Client
}

// HandleClineLogin starts and observes the official Cline device authorization
// flow. The handler is registered behind the administrator session middleware.
func (a *API) HandleClineLogin(w http.ResponseWriter, r *http.Request) {
	routeBrowserLogin(w, r, "/api/cline/login", a.startClineLogin, a.getClineLogin, a.cancelClineLogin)
}

func (a *API) startClineLogin(w http.ResponseWriter, r *http.Request) {
	enabled, ok := beginBrowserLogin(w, r, a, "Cline", a.clineLogins)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := a.config.Load()
	factory := clineLoginClientFactory()
	client := factory(nil, cfg)
	defer client.Close()
	transaction, err := client.StartLogin(ctx)
	if err != nil {
		writeAuthorizationStartFailure(w, err, authorizationFailure{
			channel:           "Cline",
			unreachableDetail: "the server cannot reach the Cline authorization endpoint",
			unavailable:       cline.ErrAuthUnavailable,
			rejected:          cline.ErrAuthRejected,
		})
		return
	}

	expiresAt := transaction.ExpiresAt
	if expiresAt.IsZero() || expiresAt.After(time.Now().Add(clineLoginTTL)) {
		expiresAt = time.Now().Add(clineLoginTTL)
	}

	admitBrowserLogin(w, a.clineLogins, "Cline", loginSeed{
		// The device code lives in the shared state because the registry's
		// readiness test requires it, and the polling response never serializes
		// it: deviceLoginResponse carries only what the browser needs, so the
		// exchangeable half cannot reach the console.
		deviceCode: transaction.DeviceCode,
		// Only the user code and the official page travel to the browser.
		userCode:   transaction.UserCode,
		verifyURI:  transaction.VerifyURL,
		verifyFull: transaction.VerifyFull,
		expiresAt:  expiresAt,
		interval:   clineLoginInterval,
		message:    "Waiting for Cline authorization",
		enabled:    enabled,
		cfg:        cfg,
	}, func(shared deviceLogin) *clineLoginTransaction {
		return &clineLoginTransaction{
			deviceLogin: shared,
			factory:     factory,
		}
	}, a.pollClineLogin)
}

func (a *API) getClineLogin(w http.ResponseWriter, id string) {
	respondLoginStatus(w, a.clineLogins, id, "Cline")
}

func (a *API) cancelClineLogin(w http.ResponseWriter, id string) {
	abandonLogin(w, a.clineLogins, id, "Cline")
}

// pollClineLogin exchanges the device grant until the browser step completes,
// then verifies and persists the account.
func (a *API) pollClineLogin(ctx context.Context, id string) {
	login, ok := a.clineLogins.pollable(id)
	if !ok {
		return
	}
	client := login.factory(nil, login.configSnapshot)
	defer client.Close()

	for {
		login, ok := awaitLoginPoll(ctx, a.clineLogins, id, "Cline")
		if !ok {
			return
		}

		reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
		creds, err := client.PollLogin(reqCtx, &cline.LoginTransaction{
			DeviceCode: login.deviceCode,
			ExpiresAt:  login.expiresAt,
		})
		reqCancel()
		if err != nil {
			if errors.Is(err, cline.ErrAuthPending) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// Authorization itself failed (the transaction was consumed,
			// cancelled, or rejected). Report it without echoing upstream text.
			slog.Warn("Cline authorization failed", "login_id", id, "error", err)
			a.clineLogins.finish(id, "failed", "Cline authorization failed; start again", 0)
			return
		}

		account, err := a.buildClineAccountFromCredentialsWithFactory(ctx, id, creds, login.configSnapshot, login.factory)
		if err != nil {
			// The reason is carried to the operator because a credential that
			// arrived but could not be persisted needs one specific action, and
			// "could not be verified" alone does not say which. The error text is
			// built from upstream status/path/business code only; no credential
			// or device code ever enters it.
			slog.Warn("Cline authorization succeeded but the account could not be stored", "login_id", id, "error", err)
			a.clineLogins.finish(id, "failed",
				"Cline authorization succeeded but the account could not be saved: "+truncateLoginReason(err), 0)
			return
		}
		if ctx.Err() != nil || !a.clineLogins.pending(id) {
			return
		}
		if login.enabledKnown {
			account.Enabled = login.enabled
		}
		finishBrowserLogin(a, ctx, a.clineLogins, id, "Cline", account, func(acc *store.Account) {
			acc.ReplaceClineCredentials = true
		})
		return
	}
}

// buildClineAccountFromCredentials turns a completed login into an account
// record.
//
// What makes a login succeed is the exchange: the WorkOS grant was traded for a
// Cline refresh token, and that token is what can renew the account. The model
// feed read is enrichment, not a credential check, so a feed that does not
// answer must not cost the operator a valid credential.
func (a *API) buildClineAccountFromCredentialsWithFactory(ctx context.Context, loginID string, creds cline.Credentials, cfg *config.Config, factory func(*store.Account, *config.Config) *cline.Client) (*store.Account, error) {
	normalized := normalizeClineLoginResult(creds)
	acc := &store.Account{
		Name:              "cline-login",
		AccountType:       "cline",
		ClineAccessToken:  normalized.AccessToken,
		ClineRefreshToken: normalized.RefreshToken,
		ClineExpiresAt:    normalized.ExpiresAt,
		ClineEmail:        normalized.Email,
		Weight:            1,
		Enabled:           true,
	}
	if normalized.Email != "" {
		acc.Email = normalized.Email
		acc.Name = normalized.Email
	}

	client := factory(acc, cfg)
	defer client.Close()

	// The catalog is read from the upstream feed at login. A failed read leaves
	// the snapshot empty on purpose: the account is saved and a later refresh
	// records the catalog, but nothing compiled in is installed as if it had been
	// observed.
	if models, catalogErr := client.FetchUpstreamModels(ctx); catalogErr != nil {
		slog.Warn("Cline upstream catalog read failed at login; leaving the snapshot empty",
			"login_id", loginID, "error", catalogErr)
	} else {
		acc.ClineModelIDs = cline.CatalogSnapshot(models)
	}

	// Without a refresh token the account cannot survive its first token expiry,
	// and the login would look successful until it silently dies.
	if strings.TrimSpace(acc.ClineRefreshToken) == "" {
		return nil, errors.New("cline login returned no refresh token")
	}
	return acc, nil
}

// normalizeClineLoginResult trims the credential a completed login produced.
func normalizeClineLoginResult(creds cline.Credentials) cline.Credentials {
	accessToken, refreshToken, expiresAt, email := creds.Fields()
	return cline.Credentials{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Email:        email,
	}
}
