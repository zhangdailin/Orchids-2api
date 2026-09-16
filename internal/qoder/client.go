package qoder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// Client is one Qoder account's upstream client.
//
// A client is cheap and stateless apart from the credential it was built from
// and the derived runtime fields, so the handler caches one per account and
// rebuilds it when the account changes.
type Client struct {
	// endpoints are resolved once from configuration so a request never has to
	// consult the config snapshot again.
	endpoints endpoints
	// clientID and clientVersion identify the CLI build this channel emulates.
	clientID      string
	clientVersion string
	// machineID is the device identity. It is bound to the credential: the
	// upstream rejects a request whose machine id did not perform the login.
	machineID string

	// control answers the short control-plane calls (token, profile, catalog).
	// stream answers the chat call, and has no client-level deadline so a long
	// generation is not cut off by the request timeout.
	control *http.Client
	stream  *http.Client

	// account is the record this client was built from. It is replaced in place
	// when the credential rotates, so the next request on this client does not
	// replay a consumed refresh token.
	account      *store.Account
	accountStore AccountUpdater

	// creds is the resolved credential. runtime holds the derived auth pair.
	creds   Credentials
	runtime RuntimeFields

	requestTimeout time.Duration

	entropy source

	refreshMu            sync.Mutex
	runtimeMu            sync.Mutex
	stateMu              sync.RWMutex
	credsDirty           bool
	dirtyExpectedRefresh string
}

// AccountUpdater is the subset of the account store the client needs to persist
// a rotated refresh token and a derived runtime pair. It is satisfied by
// *store.Store.
type AccountUpdater interface {
	UpdateAccount(ctx context.Context, acc *store.Account) error
}

type accountPatcher interface {
	UpdateQoderAccount(ctx context.Context, id int64, patch store.QoderAccountPatch) error
}

// NewFromAccount builds a client for one account. cfg supplies endpoints, proxy
// and timeout settings; the account supplies the credentials.
func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
	}
	proxyFunc := http.ProxyFromEnvironment
	proxyKey := "direct"
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
		proxyKey = util.GenerateProxyKeyFromConfig(cfg)
	}

	client := &Client{
		endpoints:      resolveEndpoints(cfg),
		clientID:       resolveClientID(cfg),
		clientVersion:  resolveClientVersion(cfg),
		control:        util.GetSharedHTTPClient(proxyKey, authRequestTimeout, proxyFunc),
		stream:         util.GetSharedHTTPClient(proxyKey+"|qoder-chat", 0, proxyFunc),
		requestTimeout: timeout,
		entropy:        cryptoSource{},
	}
	if acc != nil {
		copied := *acc
		copied.QoderOrganizationTags = append([]string(nil), acc.QoderOrganizationTags...)
		copied.QoderModelIDs = append([]string(nil), acc.QoderModelIDs...)
		client.account = &copied
	}
	client.creds = ResolveCredentials(client.account)
	if client.machineID == "" {
		// An account-less or freshly completed credential may not carry the
		// device identity yet; the resolved credential is the other place it
		// travels, and a login result always sets it.
		client.machineID = strings.TrimSpace(client.creds.MachineID)
	}
	if acc != nil {
		client.machineID = strings.TrimSpace(acc.QoderMachineID)
		client.runtime = RuntimeFields{
			EncryptUserInfo: strings.TrimSpace(acc.QoderRuntimeInfo),
			Key:             strings.TrimSpace(acc.QoderRuntimeKey),
		}
	}
	return client
}

// SetAccountStore lets the client persist a rotated refresh token and a newly
// derived runtime pair. Without it a rotation would be lost at the next
// restart, which would break the account.
func (c *Client) SetAccountStore(s AccountUpdater) {
	if c == nil {
		return
	}
	c.stateMu.Lock()
	c.accountStore = s
	c.stateMu.Unlock()
}

// Close satisfies the shared upstream client lifecycle. The HTTP transports are
// process-wide, so a client owns no resources to close.
func (c *Client) Close() {}

// SendRequestWithPayload streams one chat completion to the caller.
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	if err != nil {
		return err
	}
	model, err := c.resolveModel(req)
	if err != nil {
		return err
	}

	requestID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	sessionID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	body, err := buildChatBody(req, model, sessionID, requestID)
	if err != nil {
		return err
	}

	url := chatURL(c.endpoints.inference)
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "qoder", "model": model.Key}, body)
	}

	toolsEnabled := !req.NoTools && len(normalizeToolDefinitions(req, model)) > 0
	return c.runChat(ctx, url, body, model, requestID, fields, toolsEnabled, onMessage)
}

// runChat performs the upstream call with the CLI's retry policy: transport
// errors and retryable statuses are retried with backoff, and a 401 on the
// first attempt — and only the first — forces one token refresh.
//
// Retrying after output has been handed to the caller would duplicate content,
// so a retry is only attempted while the callback has not seen anything.
func (c *Client) runChat(ctx context.Context, url string, body []byte, model modelEntry, requestID string, fields RuntimeFields, toolsEnabled bool, onMessage func(upstream.SSEMessage)) error {
	const maxAttempts = 4
	emitted := false
	emit := func(msg upstream.SSEMessage) {
		emitted = true
		if onMessage != nil {
			onMessage(msg)
		}
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptCredentials := c.currentCredentials()
		result, err := c.attemptChat(ctx, url, body, model, requestID, fields, attemptCredentials, toolsEnabled, emit)
		if err == nil {
			if !result.SawMeaningfulEvent {
				return fmt.Errorf("qoder stream produced no usable events")
			}
			if onMessage != nil {
				event := map[string]interface{}{"finishReason": result.FinishReason()}
				if len(result.Usage) > 0 {
					event["usage"] = result.Usage
				}
				onMessage(upstream.SSEMessage{Type: "model.finish", Event: event})
			}
			return nil
		}
		lastErr = err
		if emitted {
			// Content already reached the client; replaying would duplicate it.
			return err
		}
		var agentErr *agentLimitError
		if errors.As(err, &agentErr) {
			// agentLimitResetTime is emitted by the inference agent and is not an
			// authoritative account credit snapshot.  The OpenAPI quota endpoint
			// may still report spendable credits, so do not overwrite QoderQuota or
			// globally quarantine the account here.  The handler records a
			// model-scoped cooldown; periodic/manual quota sync remains the sole
			// authority for account-wide exhaustion.
			return err
		}

		switch {
		case isUnauthorized(err) && attempt == 1:
			if refreshErr := c.forceRefresh(ctx, attemptCredentials); refreshErr != nil {
				return refreshErr
			}
			if fields, err = c.ensureRuntimeFields(ctx, c.currentCredentials()); err != nil {
				return err
			}
			continue
		case isRetryable(err) && attempt < maxAttempts:
			wait := retryDelay(err, attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		default:
			return err
		}
	}
	return lastErr
}

// attemptStreamError carries the retry decision an attempt reached.
type attemptStreamError struct {
	err       error
	retryable bool
	unauth    bool
	busy      bool
	wait      time.Duration
}

func (e *attemptStreamError) Error() string { return e.err.Error() }
func (e *attemptStreamError) Unwrap() error { return e.err }

func isUnauthorized(err error) bool {
	var target *attemptStreamError
	return asAttemptError(err, &target) && target.unauth
}

func isRetryable(err error) bool {
	var target *attemptStreamError
	return asAttemptError(err, &target) && (target.retryable || target.busy)
}

func retryDelay(err error, attempt int) time.Duration {
	var target *attemptStreamError
	if asAttemptError(err, &target) && target.wait > 0 {
		return target.wait
	}
	// 1s, 2s, 4s.
	wait := time.Duration(1<<uint(attempt-1)) * time.Second
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

func asAttemptError(err error, target **attemptStreamError) bool {
	for err != nil {
		if typed, ok := err.(*attemptStreamError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == err {
			return false
		}
		err = next
	}
	return false
}

// ensureAccessToken returns a usable device access token, refreshing when the
// stored one is missing or close to expiry.
func (c *Client) ensureAccessToken(ctx context.Context) (Credentials, error) {
	creds, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistCredentials(ctx, creds, expectedRefresh); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(creds)
	}
	now := time.Now()
	if creds.AccessValid(now) {
		return creds, nil
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) != "" {
			// Only an access token exists and it is close to expiry. Use it
			// rather than failing outright: a stale clock or an opaque token
			// without an expiry is common, and the upstream is the authority.
			return creds, nil
		}
		return Credentials{}, ErrCredentialMissing
	}
	if !creds.RefreshExpiresAt.IsZero() && now.After(creds.RefreshExpiresAt) {
		return Credentials{}, ErrReLoginRequired
	}
	return c.refresh(ctx, creds)
}

// forceRefresh renews the credential unconditionally. The stream path calls it
// after the upstream rejected a request that was otherwise well formed.
func (c *Client) forceRefresh(ctx context.Context, rejected Credentials) error {
	if strings.TrimSpace(rejected.RefreshToken) == "" {
		return ErrCredentialMissing
	}
	_, err := c.refresh(ctx, rejected)
	return err
}

// refresh renews the device credential and persists the rotation.
func (c *Client) refresh(ctx context.Context, previous Credentials) (Credentials, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Another goroutine may have refreshed while this one waited.
	current, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistCredentials(ctx, current, expectedRefresh); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(current)
	}
	if current.AccessValid(time.Now()) && current.AccessToken != previous.AccessToken {
		return current, nil
	}

	refreshed, err := c.Refresh(ctx, previous.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	// The refresh answer carries no identity, so the previous one is kept: the
	// profile fields do not change between token rotations.
	merged := Credentials{
		AccessToken:      refreshed.AccessToken,
		RefreshToken:     firstNonEmptyToken(refreshed.RefreshToken, previous.RefreshToken),
		AccessExpiresAt:  refreshed.AccessExpiresAt,
		RefreshExpiresAt: refreshed.RefreshExpiresAt,
		UID:              firstNonEmptyToken(refreshed.UID, previous.UID),
		Name:             firstNonEmptyToken(refreshed.Name, previous.Name),
		Email:            previous.Email,
		OrgID:            previous.OrgID,
		OrgTags:          previous.OrgTags,
	}
	c.storeCredentials(merged, true, previous.RefreshToken)
	if err := c.persistCredentials(ctx, merged, previous.RefreshToken); err != nil {
		return Credentials{}, err
	}
	c.markCredentialsClean(merged)
	return merged, nil
}

// ensureRuntimeFields returns the derived authentication pair, deriving and
// persisting it on first use.
//
// The pair is per account rather than per request because the CLI reuses the
// pair it generated at login; it is the account's runtime fields, and the
// upstream treats a rotation as new device material.
func (c *Client) ensureRuntimeFields(ctx context.Context, creds Credentials) (RuntimeFields, error) {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if fields := c.runtimeSnapshot(); fields.Complete() {
		return fields, nil
	}
	if strings.TrimSpace(creds.UID) == "" {
		// The runtime fields encrypt the UID, so they cannot be derived before
		// the identity is known.
		return RuntimeFields{}, fmt.Errorf("qoder account has no user id yet; sign in again")
	}
	fields, err := runtimeFieldsFor(c.entropy, runtimeFieldInput{
		UID:              creds.UID,
		OrganizationID:   creds.OrgID,
		OrganizationTags: creds.OrgTags,
		DataPolicyAgreed: c.dataPolicyAgreed(),
	})
	if err != nil {
		return RuntimeFields{}, err
	}
	if err := c.persistPatch(ctx, store.QoderAccountPatch{
		RuntimeInfo: fields.EncryptUserInfo,
		RuntimeKey:  fields.Key,
		UserID:      creds.UID,
	}); err != nil {
		return RuntimeFields{}, err
	}
	c.stateMu.Lock()
	c.runtime = fields
	if c.account != nil {
		c.account.QoderRuntimeInfo = fields.EncryptUserInfo
		c.account.QoderRuntimeKey = fields.Key
		if strings.TrimSpace(c.account.QoderUserID) == "" {
			c.account.QoderUserID = creds.UID
		}
	}
	c.stateMu.Unlock()
	return fields, nil
}

// dataPolicyAgreed reports the recorded agreement. An account created through
// this channel always agreed during the browser step, and an account whose
// agreement is unknown reports disagreement, which the gateway accepts.
func (c *Client) dataPolicyAgreed() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account == nil {
		return true
	}
	return c.account.QoderDataPolicy
}

// resolveModel maps the client's model name onto a catalog entry.
func (c *Client) resolveModel(req upstream.UpstreamRequest) (modelEntry, error) {
	catalog := c.loadCatalog()
	entry, err := catalog.Resolve(req.Model)
	if err != nil {
		return modelEntry{}, err
	}
	return entry, nil
}

// loadCatalog returns the account's observed catalog. It never performs I/O: a
// chat request must not depend on a catalog read, and the handler refreshes the
// catalog out of band.
//
// With no observation the catalog is empty on purpose. Resolving against a
// compiled-in list would accept a model the account never advertised; an empty
// catalog makes Resolve report ErrNoUpstreamCatalog instead.
func (c *Client) loadCatalog() *Catalog {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account != nil {
		if catalog := catalogFromIDs(c.account.QoderModelIDs); catalog.Len() > 0 {
			return catalog
		}
	}
	return newCatalog(nil)
}

// currentCredentials returns the live credential snapshot.
func (c *Client) currentCredentials() Credentials {
	creds, _, _ := c.currentCredentialState()
	return creds
}

func (c *Client) currentCredentialState() (Credentials, bool, string) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account == nil {
		return c.creds, c.credsDirty, c.dirtyExpectedRefresh
	}
	resolved := ResolveCredentials(c.account)
	if resolved.HasCredential() {
		return resolved, c.credsDirty, c.dirtyExpectedRefresh
	}
	return c.creds, c.credsDirty, c.dirtyExpectedRefresh
}

// storeCredentials replaces the in-memory snapshot in place.
func (c *Client) storeCredentials(creds Credentials, dirty bool, expectedRefreshToken string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.creds = creds
	c.credsDirty = dirty
	c.dirtyExpectedRefresh = expectedRefreshToken
	if c.account == nil {
		return
	}
	c.account.QoderAccessToken = creds.AccessToken
	c.account.QoderRefreshToken = creds.RefreshToken
	c.account.QoderExpiresAt = creds.AccessExpiresAt
	if creds.UID != "" {
		c.account.QoderUserID = creds.UID
	}
}

func (c *Client) markCredentialsClean(creds Credentials) {
	c.stateMu.Lock()
	if c.creds.AccessToken == creds.AccessToken && c.creds.RefreshToken == creds.RefreshToken {
		c.credsDirty = false
		c.dirtyExpectedRefresh = ""
	}
	c.stateMu.Unlock()
}

func (c *Client) runtimeSnapshot() RuntimeFields {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.runtime
}

func (c *Client) persistCredentials(ctx context.Context, creds Credentials, expectedRefreshToken string) error {
	return c.persistPatch(ctx, store.QoderAccountPatch{
		ExpectedRefreshToken: expectedRefreshToken,
		AccessToken:          creds.AccessToken,
		RefreshToken:         creds.RefreshToken,
		ExpiresAt:            creds.AccessExpiresAt,
		UserID:               creds.UID,
	})
}

// persistPatch writes only Qoder-owned fields. Production stores implement the
// atomic patch API; the full-account fallback keeps lightweight test stores
// source compatible without weakening the real persistence path.
func (c *Client) persistPatch(ctx context.Context, patch store.QoderAccountPatch) error {
	c.stateMu.RLock()
	accountStore := c.accountStore
	if c.account == nil {
		c.stateMu.RUnlock()
		return nil
	}
	acc := *c.account
	acc.QoderOrganizationTags = append([]string(nil), c.account.QoderOrganizationTags...)
	acc.QoderModelIDs = append([]string(nil), c.account.QoderModelIDs...)
	c.stateMu.RUnlock()
	if accountStore == nil || acc.ID == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if patcher, ok := accountStore.(accountPatcher); ok {
		if err := patcher.UpdateQoderAccount(writeCtx, acc.ID, patch); err != nil {
			return fmt.Errorf("persist qoder account state: %w", err)
		}
		return nil
	}
	if patch.AccessToken != "" {
		acc.QoderAccessToken = patch.AccessToken
	}
	if patch.RefreshToken != "" {
		acc.QoderRefreshToken = patch.RefreshToken
	}
	if !patch.ExpiresAt.IsZero() {
		acc.QoderExpiresAt = patch.ExpiresAt
	}
	if patch.UserID != "" {
		acc.QoderUserID = patch.UserID
	}
	if patch.RuntimeInfo != "" {
		acc.QoderRuntimeInfo = patch.RuntimeInfo
	}
	if patch.RuntimeKey != "" {
		acc.QoderRuntimeKey = patch.RuntimeKey
	}
	if patch.ModelIDs != nil {
		acc.QoderModelIDs = append([]string(nil), patch.ModelIDs...)
	}
	if err := accountStore.UpdateAccount(writeCtx, &acc); err != nil {
		return fmt.Errorf("persist qoder account state: %w", err)
	}
	return nil
}

// VerifyModel proves the account can actually run one model by issuing a
// minimal streaming completion. Model refresh uses the catalog instead, so this
// is reserved for an explicit single-model probe.
func (c *Client) VerifyModel(ctx context.Context, modelID string) error {
	return c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{
		Model:    strings.TrimSpace(modelID),
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "ping"}}},
	}, nil, nil)
}

// PrepareRuntimeFields derives the authentication pair if it is not present yet.
// It exists so a login can prove the derivation works before the credential is
// persisted, instead of surfacing the failure on the first chat request.
func (c *Client) PrepareRuntimeFields(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	_, err := c.ensureRuntimeFields(ctx, c.currentCredentials())
	return err
}

// RuntimeFields returns the derived pair. It is empty until the pair has been
// prepared.
func (c *Client) RuntimeFields() RuntimeFields {
	if c == nil {
		return RuntimeFields{}
	}
	return c.runtimeSnapshot()
}

// MachineID returns the device identity this client binds its requests to.
func (c *Client) MachineID() string {
	if c == nil {
		return ""
	}
	return c.machineID
}

// NormalizeLoginResult folds a device flow result and the machine id it was
// authorized under onto the field set the account record owns. It is the single
// mapping used by the admin login flow, so a credential written through any path
// lands in the same columns.
func NormalizeLoginResult(creds Credentials, machineID string) Credentials {
	out := creds
	out.AccessToken = strings.TrimSpace(out.AccessToken)
	out.RefreshToken = strings.TrimSpace(out.RefreshToken)
	out.UID = strings.TrimSpace(out.UID)
	out.Name = strings.TrimSpace(out.Name)
	out.Email = strings.TrimSpace(out.Email)
	out.OrgID = strings.TrimSpace(out.OrgID)
	out.MachineID = strings.TrimSpace(machineID)
	if out.MachineID == "" {
		out.MachineID = strings.TrimSpace(creds.MachineID)
	}
	return out
}

// CatalogSnapshot renders a catalog as the account's stored snapshot.
func CatalogSnapshot(catalog *Catalog) []string {
	return catalogToIDs(catalog)
}
