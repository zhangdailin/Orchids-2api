package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/egress"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Build CLI (cli-chat-proxy.grok.com) upstream. It speaks the standard OpenAI
// Responses protocol authenticated with a Bearer OAuth access token, unlike the
// app-chat website protocol (SSO cookie) or console.x.ai (SSO + DPoP).

const (
	defaultCLIBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// cli-chat-proxy rejects generic boolean token-auth headers. These values
	// mirror the official Grok shell identity used with Build OAuth tokens.
	defaultCLITokenAuth  = "xai-grok-cli"
	defaultCLIClientMode = "headless"
)

// CLIClient implements the Grok Build CLI upstream protocol.
type CLIClient struct {
	cfg        *config.Config
	httpClient *http.Client
	oauth      *CLIOAuth
	egress     *egress.Manager
}

// NewCLIClient builds a CLI upstream client from configuration.
func NewCLIClient(cfg *config.Config) *CLIClient {
	client := &CLIClient{cfg: cfg}
	// Shared browser client keeps the utls Chrome TLS fingerprint; the CLI
	// upstream tolerates browser-like TLS even though headers are CLI identity.
	client.httpClient = util.GetSharedBrowserHTTPClientWithHeaderTimeout("cli", cfg.GrokRequestTimeout(ProviderBuild), 0, nil)
	client.oauth = NewCLIOAuth(cfg, client.httpClient)
	client.egress = egress.NewManager(cfg)
	return client
}

// SetAccountStore wires a durable store so OAuth token refreshes performed on
// the request path are written back (including rotated refresh tokens).
func (c *CLIClient) SetAccountStore(s *store.Store) {
	if c == nil || c.oauth == nil {
		return
	}
	c.oauth.SetAccountStore(s)
}

// baseURL returns the CLI proxy base URL (defaults to the official gateway).
func (c *CLIClient) baseURL() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIBaseURLOrDefault()
	}
	return defaultCLIBaseURL
}

// OAuthAccessToken returns a valid access token for the account, refreshing it
// in memory when needed. Callers persist the mutated account fields.
func (c *CLIClient) OAuthAccessToken(ctx context.Context, acc *store.Account) (string, error) {
	if c == nil || c.oauth == nil {
		return "", fmt.Errorf("grok cli oauth not configured")
	}
	return c.oauth.AccessToken(ctx, acc)
}

// cliHeaders builds the Build CLI request headers for an OAuth account.
func (c *CLIClient) cliHeaders(acc *store.Account, accessToken string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("X-XAI-Token-Auth", defaultCLITokenAuth)
	h.Set("x-grok-client-version", c.clientVersion())
	h.Set("x-grok-client-identifier", c.clientIdentifier())
	h.Set("x-grok-client-mode", defaultCLIClientMode)
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip")
	h.Set("User-Agent", c.userAgent())
	h.Set("x-xai-request-id", randomUUID())
	if acc != nil {
		if teamID := strings.TrimSpace(acc.TeamID); teamID != "" {
			h.Set("x-grok-team-id", teamID)
		}
		if userID := strings.TrimSpace(acc.UserID); userID != "" {
			h.Set("x-grok-user-id", userID)
			h.Set("x-userid", userID) // Billing's alias must also use the refreshed identity.
		}
	}
	return h
}

func (c *CLIClient) userAgent() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIUserAgentOrDefault()
	}
	return "grok-shell/1.0.4 (linux; x86_64)"
}

func (c *CLIClient) clientVersion() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIClientVersionOrDefault()
	}
	return "1.0.4"
}

func (c *CLIClient) clientIdentifier() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIClientIdentifierOrDefault()
	}
	return "grok-shell"
}

// doResponses issues a standard Responses request to the CLI proxy. It ensures a
// valid access token (refreshing if needed) then POSTs the payload, returning
// the raw upstream response (SSE or JSON) for the caller to stream/collect. A
// confirmed Cloudflare challenge invalidates the egress clearance and retries at
// most once.
func (c *CLIClient) doResponses(ctx context.Context, acc *store.Account, payload map[string]interface{}) (*http.Response, error) {
	return c.doResponsesAt(ctx, acc, "/responses", payload)
}

func (c *CLIClient) doResponsesAt(ctx context.Context, acc *store.Account, path string, payload map[string]interface{}) (*http.Response, error) {
	if acc == nil {
		return nil, fmt.Errorf("empty cli account")
	}
	ctx = withRateLimitAccount(ctx, acc)
	modelID := strings.TrimSpace(fmt.Sprint(payload["model"]))
	if err := waitScopedRateLimit(ctx, ProviderBuild, acc.OAuthAccessToken, modelID, c.cfg.GrokRequestsPerSecond(ProviderBuild)); err != nil {
		return nil, err
	}
	challengeRetried := false
	authRetried := false
	for {
		resp, err := c.doResponsesOnceAt(ctx, acc, path, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		raw, headerCopy := readBoundedResponse(resp)
		if resp.StatusCode == http.StatusUnauthorized && !authRetried && c.oauth != nil && strings.TrimSpace(acc.OAuthRefreshToken) != "" {
			authRetried = true
			if _, refreshErr := c.oauth.ForceRefresh(ctx, acc); refreshErr == nil {
				continue
			}
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			if meta := noteScopedRateLimit(ctx, ProviderBuild, acc.OAuthAccessToken, modelID, resp.StatusCode, resp.Header, raw); meta != nil {
				// Keep the selected account's durable diagnostic state in sync. The
				// in-memory team/model registry remains authoritative for waiting;
				// this timestamp is only for admin visibility and restart diagnostics.
				cooldownUntil := time.Now().Add(meta.RetryAfter)
				if meta.RetryAfter > 0 && (acc.QuotaResetAt.IsZero() || cooldownUntil.After(acc.QuotaResetAt)) {
					acc.QuotaResetAt = cooldownUntil
					// The throttle is scoped to the model that was asked for: the
					// account's OTHER models stay selectable, so the verdict is
					// recorded per model instead of on StatusCode.
					store.RecordModelCooldown(acc, modelID, cooldownUntil)
					if c.oauth != nil && c.oauth.store != nil && acc.ID != 0 {
						if err := c.oauth.store.UpdateAccount(ctx, acc); err != nil {
							slog.Warn("grok cli: failed to persist team cooldown diagnostic", "account_id", acc.ID, "error", err)
						}
					}
				}
			}
			recordCLIUpstreamStatus(resp.StatusCode)
			return nil, newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
		}

		kind := ClassifyUpstreamResponse(resp.StatusCode, resp.Header, raw)
		if kind == UpstreamErrorCloudflareChallenge {
			recordUpstreamChallenge("cloudflare")
			if c.egress != nil && c.egress.Enabled() && !challengeRetried {
				challengeRetried = true
				c.egress.InvalidateAffinityClearance("cli", "cli-default")
				continue
			}
			if c.egress != nil && c.egress.Enabled() {
				c.egress.FeedbackAffinityOutcome("cli", "cli-default", egress.OutcomeChallenge)
			}
		} else if kind == UpstreamErrorDPoPChallenge {
			recordUpstreamChallenge("dpop")
		} else if kind == UpstreamErrorGenericForbidden {
			recordGenericForbidden()
		}

		recordCLIUpstreamStatus(resp.StatusCode)
		return nil, newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
	}
}

func (c *CLIClient) doResponsesOnceAt(ctx context.Context, acc *store.Account, path string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	headers := http.Header{"Content-Type": {"application/json"}}
	if strings.HasPrefix(path, "/videos/") {
		model, _ := payload["model"].(string)
		headers.Set("x-grok-model-override", firstNonEmpty(strings.TrimSpace(model), "grok-imagine-video-1.5"))
	}
	if session, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(session) != "" {
		headers.Set("x-grok-session-id", strings.TrimSpace(session))
		headers.Set("x-grok-conv-id", strings.TrimSpace(session))
	}
	return c.request(ctx, acc, http.MethodPost, c.baseURL()+path, body, headers)
}

// doFallbackRequest sends a request to the direct xAI API with the same
// refreshed OAuth credential and fail-closed egress policy as the Build path.
func (c *CLIClient) doFallbackRequest(ctx context.Context, acc *store.Account, method, path string, payload map[string]interface{}) (*http.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("grok cli client not configured")
	}
	var body []byte
	headers := http.Header{}
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		headers.Set("Content-Type", "application/json")
	}
	base := c.cfg.GrokCLIFallbackBaseURLOrDefault()
	resp, err := c.request(ctx, acc, method, strings.TrimRight(base, "/")+"/"+strings.TrimLeft(strings.TrimSpace(path), "/"), body, headers)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	raw, responseHeaders := readBoundedResponse(resp)
	return nil, newCLIUpstreamError(resp.StatusCode, responseHeaders, raw)
}

// doResponseResource forwards GET/DELETE for a stored Build Responses
// resource. Non-2xx statuses are returned intact so the downstream API can
// preserve the upstream resource semantics.
func (c *CLIClient) doResponseResource(ctx context.Context, acc *store.Account, method, path, rawQuery string) (*http.Response, error) {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	endpoint := c.baseURL() + path
	if strings.TrimSpace(rawQuery) != "" {
		endpoint += "?" + rawQuery
	}
	headers := http.Header{}
	if strings.HasPrefix(path, "/videos/") {
		headers.Set("x-grok-model-override", "grok-imagine-video-1.5")
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.request(ctx, acc, method, endpoint, nil, headers)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 || strings.TrimSpace(acc.OAuthRefreshToken) == "" {
			return resp, nil
		}
		_ = resp.Body.Close()
		if _, err := c.oauth.ForceRefresh(ctx, acc); err != nil {
			return nil, err
		}
	}
}

// VerifyAccount checks a Build CLI OAuth account by minting a token and probing
// the CLI proxy models endpoint. Returns an upstream status string ("401",
// "403", ...) alongside the error so callers can mark the account. A confirmed
// Cloudflare challenge invalidates egress clearance and retries once.
func (c *CLIClient) VerifyAccount(ctx context.Context, acc *store.Account) (string, error) {
	if c == nil || c.oauth == nil {
		return "", fmt.Errorf("grok cli client not configured")
	}
	if acc == nil {
		return "", fmt.Errorf("missing cli account")
	}
	challengeRetried := false
	for {
		resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/models", nil, nil)
		if err != nil {
			if oauthErr, ok := err.(*cliOAuthError); ok {
				return oauthErr.Status(), err
			}
			return "", err
		}
		if resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return "", nil
		}
		raw, headerCopy := readBoundedResponse(resp)

		kind := ClassifyUpstreamResponse(resp.StatusCode, resp.Header, raw)
		if kind == UpstreamErrorCloudflareChallenge && c.egress != nil && c.egress.Enabled() && !challengeRetried {
			challengeRetried = true
			recordUpstreamChallenge("cloudflare")
			c.egress.InvalidateAffinityClearance("cli", "cli-default")
			continue
		}
		return classifyAccountStatusFromHTTP(resp.StatusCode), newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
	}
}

// FetchModels returns the official, account-scoped Build catalog. It is a
// control-plane request and never sends a model prompt. The result must be
// persisted as account capability rather than merged into a global static list.
func (c *CLIClient) FetchModels(ctx context.Context, acc *store.Account) ([]string, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli models is not configured")
	}
	ApplyCLIOAuthIdentity(acc)
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/models", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	var payload struct {
		Data []struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			ModelID string `json:"modelId"`
			Hidden  bool   `json:"hidden"`
			Meta    struct {
				Model   string `json:"model"`
				ModelID string `json:"modelId"`
				Hidden  bool   `json:"hidden"`
			} `json:"_meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode grok cli models response: %w", err)
	}
	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if item.Hidden || item.Meta.Hidden {
			continue
		}
		model := firstNonEmpty(item.ID, item.Model, item.ModelID, item.Meta.Model, item.Meta.ModelID)
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("grok cli models response contains no model ids")
	}
	return models, nil
}

// request is the single authenticated Build HTTP entry for chat, resources,
// models, billing and fallback. Callers retain their endpoint status semantics.
func (c *CLIClient) request(ctx context.Context, acc *store.Account, method, endpoint string, body []byte, headers http.Header) (*http.Response, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli client or account not configured")
	}
	token, err := c.oauth.AccessToken(ctx, acc)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("grok cli account access token is empty")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.cliHeaders(acc, token)
	for key, values := range headers {
		req.Header[key] = append([]string(nil), values...)
	}
	return doUpstreamHTTP(req, func(req *http.Request) (*http.Response, error) {
		return c.doCLIRequest(ctx, req)
	}, c.cfg.GrokStreamIdleTimeout())
}

// doCLIRequest is the fail-closed egress adapter. A successful response owns
// its lease until the shared response lifecycle closes the body.
func (c *CLIClient) doCLIRequest(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c.egress == nil || !c.egress.Enabled() {
		return c.httpClient.Do(req)
	}
	lease, err := c.egress.Acquire(ctx, "cli", "cli-default")
	if err != nil {
		recordEgressAcquireError()
		return nil, fmt.Errorf("grok cli egress unavailable: %w", err)
	}
	if lease.UserAgent != "" {
		req.Header.Set("User-Agent", lease.UserAgent)
	}
	if lease.CFCookies != "" {
		mergeCFCookies(req.Header, lease.CFCookies)
	}
	resp, err := lease.Do(req)
	if err != nil {
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeTransportError)
		lease.Release()
		return nil, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeSuccess)
	case resp.StatusCode == http.StatusTooManyRequests:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeRateLimited)
	case resp.StatusCode >= 500:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeServerError)
	default:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeForbidden)
	}
	if resp != nil && resp.Body != nil {
		resp.Body = &leaseResponseBody{ReadCloser: resp.Body, release: lease.Release}
	} else {
		lease.Release()
	}
	return resp, nil
}

func classifyAccountStatusFromHTTP(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "401"
	case http.StatusForbidden:
		return "403"
	case http.StatusTooManyRequests:
		return "429"
	case http.StatusPaymentRequired:
		return "402"
	default:
		return ""
	}
}

// cliOAuthError is a lightweight wrapper so VerifyAccount can surface the
// upstream status of a failed refresh.
type cliOAuthError struct {
	status  int
	message string
}

func (e *cliOAuthError) Error() string { return e.message }
func (e *cliOAuthError) Status() string {
	if e == nil || e.status == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.status)
}
