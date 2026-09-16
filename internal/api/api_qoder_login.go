package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

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
	w.Header().Set("Cache-Control", "no-store")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/qoder/login"), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		a.startQoderLogin(w, r)
	case r.Method == http.MethodGet && path != "":
		a.getQoderLogin(w, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelQoderLogin(w, path)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startQoderLogin(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		writeQoderLoginError(w, http.StatusServiceUnavailable, "store_unavailable",
			"account store is not configured")
		return
	}
	if !sameOriginAdminRequest(w, r, "Qoder") {
		return
	}
	options, ok := parseQoderLoginOptions(w, r)
	if !ok {
		return
	}
	enabled := true
	if options.Enabled != nil {
		enabled = *options.Enabled
	}

	a.qoderLogins.cleanup(time.Now())

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := a.config.Load()
	factory := qoderLoginClientFactory()
	client := factory(nil, cfg)
	defer client.Close()
	transaction, err := client.StartLogin(ctx)
	if err != nil {
		// Upstream failures can echo request material, so only the classified
		// cause reaches the browser; the detail goes to the server log.
		switch {
		case errors.Is(err, qoder.ErrAuthUnavailable):
			slog.Warn("Qoder authorization endpoint is unreachable", "error", err)
			writeQoderLoginError(w, http.StatusBadGateway, "upstream_unreachable",
				"the server cannot reach the Qoder authorization endpoint")
		case errors.Is(err, qoder.ErrAuthRejected):
			slog.Warn("Qoder authorization was rejected by the upstream", "error", err)
			writeQoderLoginError(w, http.StatusBadGateway, "upstream_rejected",
				"Qoder refused to start a login transaction")
		default:
			slog.Warn("Qoder authorization could not be started", "error", err)
			writeQoderLoginError(w, http.StatusBadGateway, "upstream_error",
				"failed to start Qoder authorization")
		}
		return
	}

	id, err := newDeviceLoginID()
	if err != nil {
		writeQoderLoginError(w, http.StatusInternalServerError, "transaction_failed",
			"failed to create login transaction")
		return
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &qoderLoginTransaction{
		deviceLogin: deviceLogin{
			verifyURI:      qoderVerifyURI(cfg),
			verifyFull:     transaction.VerifyURL,
			expiresAt:      transaction.ExpiresAt,
			interval:       qoderLoginInterval,
			cancel:         pollCancel,
			done:           make(chan struct{}),
			configSnapshot: cfg,
			status:         "pending",
			message:        "Waiting for Qoder authorization",
			enabled:        enabled,
			// The caller always declares the intended state, so the completed
			// account must honour it instead of defaulting to enabled.
			enabledKnown: true,
		},
		verifyURL: transaction.VerifyURL,
		nonce:     transaction.Nonce,
		verifier:  transaction.Verifier,
		machineID: transaction.MachineID,
		factory:   factory,
	}

	if !a.qoderLogins.admit(id, login) {
		pollCancel()
		writeQoderLoginError(w, http.StatusTooManyRequests, "too_many_logins",
			"too many pending Qoder logins; finish or cancel one first")
		return
	}

	go func() {
		defer close(login.done)
		a.pollQoderLogin(pollContext, id)
	}()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(newDeviceLoginResponse(id, &login.deviceLogin))
}

func (a *API) getQoderLogin(w http.ResponseWriter, id string) {
	a.qoderLogins.cleanup(time.Now())
	response, ok := a.qoderLogins.response(id)
	if !ok {
		writeQoderLoginError(w, http.StatusNotFound, "login_not_found",
			"Qoder login session not found or already finished")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (a *API) cancelQoderLogin(w http.ResponseWriter, id string) {
	done, ok := a.qoderLogins.cancel(id, "Qoder authorization cancelled")
	if !ok {
		writeQoderLoginError(w, http.StatusNotFound, "login_not_found",
			"Qoder login session not found or already finished")
		return
	}
	waitForLoginPoll(done)
	w.WriteHeader(http.StatusNoContent)
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
		login, ok = a.qoderLogins.pollable(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.qoderLogins.finish(id, "expired", "Qoder authorization timed out; start again", 0)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
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
		existing, err := a.findDuplicateAccountByCredential(ctx, account, 0)
		if err != nil {
			a.qoderLogins.finish(id, "failed", "Qoder authorization succeeded but the account could not be saved", 0)
			return
		}
		if existing != nil {
			account.ID = existing.ID
			account.ReplaceQoderCredentials = true
			if err := a.store.UpdateAccount(ctx, account); err != nil {
				a.qoderLogins.finish(id, "failed", "Qoder authorization succeeded but the account could not be updated", 0)
				return
			}
			a.qoderLogins.finish(id, "complete", "Qoder account credentials refreshed", account.ID)
			return
		}
		if err := a.store.CreateAccount(ctx, account); err != nil {
			a.qoderLogins.finish(id, "failed", "Qoder authorization succeeded but the account could not be saved", 0)
			return
		}
		a.qoderLogins.finish(id, "complete", "Qoder account added", account.ID)
		a.syncAccountAfterCreate(*account)
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
	}

	// Record the allowance at login so the account table can show the plan and
	// the operator learns immediately whether this account can actually spend.
	// A failure here is informational: the credential is already good.
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("Qoder quota read failed at login; leaving the allowance unknown",
			"login_id", loginID, "error", quotaErr)
	} else {
		qoder.ApplyQuota(acc, quota)
	}

	if strings.TrimSpace(acc.QoderRefreshToken) == "" {
		// Without a refresh token the account cannot survive its first token
		// expiry, and the login would look successful until it silently dies.
		return nil, errors.New("qoder login returned no refresh token")
	}
	return acc, nil
}

// writeQoderLoginError reports a failure with a stable machine-readable code so
// the admin UI can explain the actual cause instead of guessing. The message is
// operator-facing and never contains credentials or upstream error text.
func writeQoderLoginError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
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

// truncateLoginReason bounds the reason shown in the console. It is deliberately
// short: the full detail is already in the server log.
func truncateLoginReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "unknown error"
	}
	const limit = 200
	if len(reason) > limit {
		reason = reason[:limit] + "..."
	}
	return reason
}

// qoderLoginOptions parses the optional start body.
type qoderLoginOptions struct {
	Enabled *bool `json:"enabled"`
}

func parseQoderLoginOptions(w http.ResponseWriter, r *http.Request) (qoderLoginOptions, bool) {
	var options qoderLoginOptions
	if r.Body == nil {
		return options, true
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return options, false
	}
	if strings.TrimSpace(string(body)) == "" {
		return options, true
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil {
		http.Error(w, "invalid login payload", http.StatusBadRequest)
		return qoderLoginOptions{}, false
	}
	return options, true
}
