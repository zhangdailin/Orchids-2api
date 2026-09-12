// Package workbuddy implements the WorkBuddy international backend
// (www.workbuddy.ai, isOversea=true) as an upstream provider.
//
// The wire protocol is OpenAI-shaped but not OpenAI-compatible in three ways
// that the client must honour:
//
//  1. messages[0] must be a system message, otherwise the upstream answers
//     HTTP 400 code=11128 ("first message is not system prompt").
//  2. stream must be true; the endpoint always answers with SSE.
//  3. tool_choice is a plain string (an object form is rejected with
//     code=11101), and the "developer" role is not in the accepted role
//     whitelist.
//
// Errors travel inside the {code,msg,requestId,data} envelope; code != 0 is a
// business error even when HTTP status is 200. 6004 is a per-model frequency
// limit (the account stays usable on other models) and 12153 means the stored
// session is dead.
package workbuddy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// DefaultBaseURL is the international deployment host.
const DefaultBaseURL = "https://www.workbuddy.ai"

const (
	defaultModel   = "default-model"
	clientUA       = "WorkBuddyAI/5.5.2 (WorkBuddyAI)"
	originReferer  = "https://www.workbuddy.ai"
	defaultSystem  = "You are a helpful assistant."
	minRefreshLead = 24 * time.Hour
)

// Business error codes observed from the international backend.
const (
	CodeLoginPending  = 11217
	CodeModelThrottle = 6004
	CodeSystemFirst   = 11128
	CodeSessionDead   = 12153
)

// Client is one WorkBuddy account's upstream client.
type Client struct {
	httpClient     *http.Client
	baseURL        string
	requestTimeout time.Duration
	account        *store.Account
	accountStore   AccountUpdater

	creds   Credentials
	updater *tokenUpdater
}

// NewFromAccount builds a client for the given account. cfg supplies proxy and
// timeout settings only; the account supplies the credentials.
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
	baseURL := DefaultBaseURL
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
		proxyKey = util.GenerateProxyKeyFromConfig(cfg)
		if override := strings.TrimSpace(cfg.WorkBuddyBaseURL); override != "" {
			baseURL = strings.TrimRight(override, "/")
		}
	}

	return &Client{
		httpClient:     util.GetSharedHTTPClient(proxyKey, timeout, proxyFunc),
		baseURL:        baseURL,
		requestTimeout: timeout,
		account:        acc,
		creds:          ResolveCredentials(acc),
	}
}

// SetAccountStore lets the client persist a rotated refresh token (Keycloak
// rotates it on every refresh, so dropping the new value breaks the account).
func (c *Client) SetAccountStore(s AccountUpdater) {
	if c == nil {
		return
	}
	c.accountStore = s
	if c.updater != nil {
		c.updater.accountStore = s
	}
}

// Close satisfies the shared upstream client lifecycle. The HTTP transport is
// process-wide, so a client owns no resources to close.
func (c *Client) Close() {}

// SendRequestWithPayload streams one chat completion to the caller.
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return c.runChat(ctx, req, c.requestTimeout, onMessage, logger)
}

func (c *Client) runChat(ctx context.Context, req upstream.UpstreamRequest, timeout time.Duration, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("workbuddy client is nil")
	}

	accessToken, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}

	body, err := c.buildBody(req)
	if err != nil {
		return err
	}

	url := c.baseURL + "/v2/chat/completions"
	if logger != nil {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "workbuddy"}, body)
	}

	reqCtx, cancel := util.WithDefaultTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("failed to create workbuddy request: %w", err)
	}
	applyHeaders(httpReq, accessToken, c.creds.UID, "text/event-stream")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to send workbuddy request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return apiError(resp.StatusCode, raw)
	}

	result, err := consumeStream(resp.Body, onMessage)
	if err != nil {
		return err
	}
	if !result.SawMeaningfulEvent {
		return fmt.Errorf("workbuddy API returned no usable stream events")
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

// ensureAccessToken returns a usable bearer token, refreshing when the stored
// access token is missing or about to expire.
func (c *Client) ensureAccessToken(ctx context.Context) (string, error) {
	if c.updater == nil {
		c.updater = newTokenUpdater(c.baseURL, c.httpClient, c.accountStore, c.account)
	}
	return c.updater.Token(ctx, c.creds)
}

// buildBody renders the OpenAI-shaped request body the upstream expects.
func (c *Client) buildBody(req upstream.UpstreamRequest) ([]byte, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}
	body := map[string]interface{}{
		"model":    model,
		"stream":   true,
		"messages": buildMessages(req),
	}
	if req.Tools != nil && !req.NoTools {
		if tools := normalizeToolDefinitions(req.Tools); len(tools) > 0 {
			body["tools"] = tools
			body["tool_choice"] = "auto"
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal workbuddy request: %w", err)
	}
	return raw, nil
}

// VerifyModel performs a minimal streaming completion to prove the account can
// actually run the model. Model refresh uses the /v3/config catalog, so this is
// reserved for explicit single-model probes.
func (c *Client) VerifyModel(ctx context.Context, modelID string) error {
	if c == nil {
		return fmt.Errorf("workbuddy client is nil")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		modelID = defaultModel
	}
	return c.runChat(ctx, upstream.UpstreamRequest{Model: modelID}, 45*time.Second, nil, nil)
}

// RefreshCredentials forces one token refresh and returns the rotated pair. It
// is used at login time to capture the durable refresh token when the
// authorization response did not include one.
func (c *Client) RefreshCredentials(ctx context.Context) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("workbuddy client is nil")
	}
	if c.updater == nil {
		c.updater = newTokenUpdater(c.baseURL, c.httpClient, c.accountStore, c.account)
	}
	return c.updater.RefreshNow(ctx, c.creds)
}
