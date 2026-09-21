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
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// The Qoder channel is OAuth-only, and the console drives the whole flow:
//
//	POST   /api/qoder/login          start a device authorization transaction
//	GET    /api/qoder/login/{id}     observe it while the operator authorizes
//	DELETE /api/qoder/login/{id}     abandon it
//
// Only the challenge and the official page URL ever reach the browser. The
// verifier, the nonce and the resulting credential stay server-side, so a
// compromised browser session cannot mint a credential from a half-finished
// transaction.

const (
	// qoderLoginInterval is the upstream poll cadence. The CLI polls every
	// second; the console polls a little slower so a stalled transaction cannot
	// hammer the token endpoint for fifteen minutes.
	qoderLoginInterval = 2 * time.Second
)

// newQoderLoginClient builds the client used by a login transaction. It is a
// variable so tests can drive the flow without touching the network.
var newQoderLoginClient = func(acc *store.Account, cfg *config.Config) *qoder.Client {
	return qoder.NewFromAccount(acc, cfg)
}

var newQoderLoginClientMu sync.RWMutex

func qoderLoginClientFactory() func(*store.Account, *config.Config) *qoder.Client {
	newQoderLoginClientMu.RLock()
	factory := newQoderLoginClient
	newQoderLoginClientMu.RUnlock()
	return factory
}

// qoderLoginTransaction is the server-side half of an in-flight login. It is
// held in the shared device-login map so the flow reuses the console's existing
// bookkeeping, but the private material never leaves this file.
//
// The nonce and verifier are deliberately NOT stored in the embedded
// deviceCode/userCode fields: those are serialized into the polling response,
// and a nonce that reached the browser would let a third party complete the
// transaction.
type qoderLoginTransaction struct {
	deviceLogin
	// verifyURL is the official page the operator must open.
	verifyURL string
	nonce     string
	verifier  string
	machineID string
	factory   func(*store.Account, *config.Config) *qoder.Client
}

// HandleQoderLogin starts and observes the official Qoder device authorization
// flow. The handler is registered behind the administrator session middleware.
func (a *API) HandleQoderLogin(w http.ResponseWriter, r *http.Request) {
	routeBrowserLogin(w, r, "/api/qoder/login", a.startQoderLogin, a.getQoderLogin, a.cancelQoderLogin)
}

func (a *API) startQoderLogin(w http.ResponseWriter, r *http.Request) {
	enabled, ok := beginBrowserLogin(w, r, a, "Qoder", a.qoderLogins)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := a.config.Load()
	factory := qoderLoginClientFactory()
	client := factory(nil, cfg)
	defer client.Close()
	transaction, err := client.StartLogin(ctx)
	if err != nil {
		writeAuthorizationStartFailure(w, err, authorizationFailure{
			channel:           "Qoder",
			unreachableDetail: "the server cannot reach the Qoder authorization endpoint",
			unavailable:       qoder.ErrAuthUnavailable,
			rejected:          qoder.ErrAuthRejected,
		})
		return
	}

	admitBrowserLogin(w, a.qoderLogins, "Qoder", loginSeed{
		verifyURI:  qoderVerifyURI(cfg),
		verifyFull: transaction.VerifyURL,
		expiresAt:  transaction.ExpiresAt,
		interval:   qoderLoginInterval,
		message:    "Waiting for Qoder authorization",
		enabled:    enabled,
		cfg:        cfg,
	}, func(shared deviceLogin) *qoderLoginTransaction {
		// The verifier and the nonce stay out of the embedded state: that part is
		// serialized into the polling response, and a nonce the browser could read
		// would let a third party complete the transaction.
		return &qoderLoginTransaction{
			deviceLogin: shared,
			verifyURL:   transaction.VerifyURL,
			nonce:       transaction.Nonce,
			verifier:    transaction.Verifier,
			machineID:   transaction.MachineID,
			factory:     factory,
		}
	}, a.pollQoderLogin)
}

func (a *API) getQoderLogin(w http.ResponseWriter, id string) {
	respondLoginStatus(w, a.qoderLogins, id, "Qoder")
}

func (a *API) cancelQoderLogin(w http.ResponseWriter, id string) {
	abandonLogin(w, a.qoderLogins, id, "Qoder")
}

// pollQoderLogin exchanges the device token until the browser step completes,
// then verifies and persists the account.
func (a *API) pollQoderLogin(ctx context.Context, id string) {
	login, ok := a.qoderLogins.pollable(id)
	if !ok {
		return
	}
	client := login.factory(nil, login.configSnapshot)
	defer client.Close()

	for {
		login, ok := awaitLoginPoll(ctx, a.qoderLogins, id, "Qoder")
		if !ok {
			return
		}

		reqCtx, reqCancel := context.WithTimeout(ctx, 30*time.Second)
		creds, err := client.PollLogin(reqCtx, &qoder.LoginTransaction{
			VerifyURL: login.verifyURL,
			Nonce:     login.nonce,
			Verifier:  login.verifier,
			MachineID: login.machineID,
			ExpiresAt: login.expiresAt,
		})
		reqCancel()
		if err != nil {
			if errors.Is(err, qoder.ErrAuthPending) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// Authorization itself failed (the transaction was consumed,
			// cancelled, or rejected). Report it without echoing upstream text.
			slog.Warn("Qoder authorization failed", "login_id", id, "error", err)
			a.qoderLogins.finish(id, "failed", "Qoder authorization failed; start again", 0)
			return
		}

		account, err := a.buildQoderAccountFromCredentialsWithFactory(ctx, id, login.machineID, creds, login.configSnapshot, login.factory)
		if err != nil {
			// The reason is carried to the operator because a credential that
			// arrived but could not be persisted needs one specific action, and
			// "could not be verified" alone does not say which. The error text is
			// built from upstream status/path/business code only; no credential,
			// verifier or nonce ever enters it.
			slog.Warn("Qoder authorization succeeded but the account could not be stored", "login_id", id, "error", err)
			a.qoderLogins.finish(id, "failed",
				"Qoder authorization succeeded but the account could not be saved: "+truncateLoginReason(err), 0)
			return
		}
		if ctx.Err() != nil || !a.qoderLogins.pending(id) {
			return
		}
		if login.enabledKnown {
			account.Enabled = login.enabled
		}
		finishBrowserLogin(a, ctx, a.qoderLogins, id, "Qoder", account, func(acc *store.Account) {
			acc.ReplaceQoderCredentials = true
		})
		return
	}
}

// buildQoderAccountFromCredentials turns a completed login into an account record.
//
// What makes a login succeed is the device credential plus a resolved identity:
// the upstream issued the token and named the account, and both are durable. The
// model list read is enrichment, not a credential check, so a gateway that does
// not answer it must not cost the operator a valid credential. The catalog is
// read from the signed control plane; when that read fails the snapshot is left
// empty, the real reason is logged, and the operator runs a model refresh.
func (a *API) buildQoderAccountFromCredentialsWithFactory(ctx context.Context, loginID, machineID string, creds qoder.Credentials, cfg *config.Config, factory func(*store.Account, *config.Config) *qoder.Client) (*store.Account, error) {
	normalized := qoder.NormalizeLoginResult(creds, machineID)
	acc := &store.Account{
		Name:              "qoder-login",
		AccountType:       "qoder",
		QoderAccessToken:  normalized.AccessToken,
		QoderRefreshToken: normalized.RefreshToken,
		QoderExpiresAt:    normalized.AccessExpiresAt,
		QoderMachineID:    normalized.MachineID,
		QoderUserID:       normalized.UID,
		QoderUserName:     normalized.Name,
		QoderDataPolicy:   true,
		Weight:            1,
		Enabled:           true,
	}
	if normalized.Email != "" {
		acc.Email = normalized.Email
	}

	client := factory(acc, cfg)
	defer client.Close()

	// The identity enrichment is optional: a userinfo outage must not invalidate
	// a credential that already works.
	if profile, err := client.FetchProfile(ctx, acc.QoderAccessToken); err != nil {
		slog.Debug("Qoder profile lookup failed; keeping the login identity", "login_id", loginID, "error", err)
	} else {
		if uid := strings.TrimSpace(profile.UID); uid != "" {
			acc.QoderUserID = uid
		}
		if name := strings.TrimSpace(profile.Name); name != "" {
			acc.QoderUserName = name
		}
		if email := strings.TrimSpace(profile.Email); email != "" {
			acc.Email = email
		}
		if orgID := strings.TrimSpace(profile.OrgID); orgID != "" {
			acc.QoderOrganizationID = orgID
		}
		acc.QoderOrganizationTags = profile.OrgTags
	}

	if strings.TrimSpace(acc.QoderUserID) == "" {
		return nil, errors.New("qoder login returned no user id")
	}
	switch {
	case strings.TrimSpace(acc.Email) != "":
		acc.Name = acc.Email
	case strings.TrimSpace(acc.QoderUserName) != "":
		acc.Name = acc.QoderUserName
	case len(acc.QoderUserID) >= 8:
		acc.Name = "qoder-" + acc.QoderUserID[:8]
	}

	// Derive the runtime auth pair now so the account is usable and the failure
	// surface is the login rather than the first chat request.
	if err := client.PrepareRuntimeFields(ctx); err != nil {
		return nil, err
	}
	acc.QoderRuntimeInfo = client.RuntimeFields().EncryptUserInfo
	acc.QoderRuntimeKey = client.RuntimeFields().Key

	// The catalog is read from the signed upstream control plane at login. A
	// failed read leaves the snapshot empty on purpose: the account is saved and
	// a later refresh records the catalog, but nothing compiled in is installed
	// as if it had been observed.
	if models, catalogErr := client.FetchUpstreamModels(ctx); catalogErr != nil {
		slog.Warn("Qoder upstream catalog read failed at login; leaving the snapshot empty",
			"login_id", loginID, "error", catalogErr)
	} else {
		acc.QoderModelIDs = qoder.CatalogSnapshot(models)
		if len(acc.QoderModelIDs) > 0 {
			acc.QoderModelsSyncedAt = time.Now()
		}
	}

	// Record the allowance at login so the account table can show the plan and
	// the operator learns immediately whether this account can actually spend.
	// A failure here is informational: the credential is already good.
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("Qoder quota read failed at login; leaving the allowance unknown",
			"login_id", loginID, "error", quotaErr)
	} else {
		qoder.ApplyQuota(acc, quota)
		if quota.Exhausted {
			acc.StatusCode = store.AccountStatusQoderQuotaExhausted
			acc.StatusMessage = "Qoder allowance exhausted; free catalog models remain eligible"
			acc.LastAttempt = time.Now()
		}
	}

	if strings.TrimSpace(acc.QoderRefreshToken) == "" {
		// Without a refresh token the account cannot survive its first token
		// expiry, and the login would look successful until it silently dies.
		return nil, errors.New("qoder login returned no refresh token")
	}
	return acc, nil
}

// qoderVerifyURI is the human-facing authorization page. It is what the console
// shows as a link, while verifyFull carries the single-use challenge.
func qoderVerifyURI(cfg *config.Config) string {
	if cfg != nil {
		if base := strings.TrimSpace(cfg.QoderOAuthBaseURL); base != "" {
			return strings.TrimRight(base, "/") + "/device/selectAccounts"
		}
	}
	return qoder.DefaultOAuthBaseURL + "/device/selectAccounts"
}
