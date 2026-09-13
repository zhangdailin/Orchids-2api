package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
	authClient *http.Client
	session    *session
}

const (
	defaultRequestTimeout = 600 * time.Second
	maxRequestTimeout     = 600 * time.Second
)

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	if acc == nil {
		httpClient := newHTTPClient(0, cfg)
		return &Client{config: cfg, httpClient: httpClient}
	}

	refresh := RefreshToken(acc)
	sess := getSession(acc.ID, refresh, acc.DeviceID, acc.RequestID)
	httpClient := newHTTPClient(0, cfg)
	authClient := newHTTPClient(0, cfg)
	httpClient.Jar = sess.jar
	authClient.Jar = sess.jar

	return &Client{
		config:     cfg,
		account:    acc,
		httpClient: httpClient,
		authClient: authClient,
		session:    sess,
	}
}

func newHTTPClient(timeout time.Duration, cfg *config.Config) *http.Client {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
		if cfg != nil && cfg.RequestTimeout > 0 {
			timeout = time.Duration(cfg.RequestTimeout) * time.Second
		}
		if timeout > maxRequestTimeout {
			timeout = maxRequestTimeout
		}
	}

	var proxyFunc func(*http.Request) (*url.URL, error)
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: newWarpTransport(proxyFunc),
	}
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	for _, client := range []*http.Client{c.httpClient, c.authClient} {
		if client == nil || client.Transport == nil {
			continue
		}
		if closer, ok := client.Transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func (c *Client) ProbeModel(ctx context.Context, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req := upstream.UpstreamRequest{
		Prompt:  "Reply with ok.",
		Model:   model,
		NoTools: true,
	}

	if _, err := c.ensureAuthenticated(ctx, true); err != nil {
		return err
	}

	_, payload, err := buildRequestBytes(req)
	if err != nil {
		return err
	}

	refresh := func() error {
		_, err := c.ensureAuthenticated(ctx, true)
		return err
	}
	return c.streamWithRetry(ctx, payload, req, func(upstream.SSEMessage) {}, nil, refresh)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	ctx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	if _, err := c.ensureAuthenticated(ctx, true); err != nil {
		return err
	}

	promptText, payload, err := buildRequestBytes(req)
	if err != nil {
		return err
	}
	if logger != nil {
		logger.LogConvertedPrompt(promptText)
	}

	defaultRefresh := func() error {
		_, err := c.ensureAuthenticated(ctx, true)
		return err
	}
	var upstreamConversationID string
	var upstreamRequestID string
	intercept := func(message upstream.SSEMessage) {
		if message.Type == "model.conversation_id" {
			if id, ok := message.Event["id"].(string); ok {
				upstreamConversationID = strings.TrimSpace(id)
			}
		}
		if message.Type == "model.request_id" {
			if id, ok := message.Event["id"].(string); ok {
				upstreamRequestID = strings.TrimSpace(id)
			}
			return
		}
		if message.Type == "model.usage-metadata" {
			if message.Event == nil {
				message.Event = make(map[string]interface{})
			}
			message.Event["requestId"] = upstreamRequestID
			message.Event["conversationId"] = upstreamConversationID
		}
		if onMessage != nil {
			onMessage(message)
		}
	}
	err = c.streamWithRetry(ctx, payload, req, intercept, logger, defaultRefresh)
	return AttachRequestMetadata(err, upstreamConversationID, upstreamRequestID)
}

func (c *Client) doStreamRequest(ctx context.Context, payload []byte, logger *debug.Logger) (*http.Response, error) {
	jwt := c.session.currentJWT()
	if jwt == "" {
		return nil, fmt.Errorf("warp jwt missing")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, warpAIURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	applyWarpClientHeaders(req)
	experimentID, experimentBucket := c.session.experimentHeaders()
	req.Header.Set("X-Warp-Experiment-Id", experimentID)
	req.Header.Set("X-Warp-Experiment-Bucket", experimentBucket)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(payload)))

	if logger != nil && !logger.Capturing() {
		headers := map[string]string{}
		for k, v := range req.Header {
			headers[k] = strings.Join(v, ", ")
		}
		logger.LogUpstreamRequest(warpAIURL, headers, payload)
	}

	var diagnosticBody interface{}
	if debug.FromContext(ctx) != nil {
		var decoded warpapi.Request
		diagnosticBody = "Protobuf request could not be decoded"
		if proto.Unmarshal(payload, &decoded) == nil {
			if raw, err := protojson.Marshal(&decoded); err == nil {
				diagnosticBody = json.RawMessage(raw)
			}
		}
	}
	attempt := debug.BeginUpstream(ctx, req.Method, warpAIURL, req.Header, diagnosticBody)
	if logger != nil {
		logger.UseUpstreamAttempt(attempt)
	}
	resp, err := c.httpClient.Do(req)
	attempt.Response(resp, err)
	return resp, err
}

func (c *Client) streamWithRetry(ctx context.Context, payload []byte, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger, refresh func() error) error {
	c.session.beginRequest()

	resp, err := c.doStreamRequest(ctx, payload, logger)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		if logger != nil && logger.Capturing() {
			body, _ := readLimitedBody(resp, 4096)
			logger.LogUpstreamHTTPError(warpAIURL, resp.StatusCode, string(body), nil)
		}
		c.session.clearToken()
		_ = resp.Body.Close()

		if err := refresh(); err != nil {
			return err
		}
		c.session.beginRequest()
		resp, err = c.doStreamRequest(ctx, payload, logger)
		if err != nil {
			return err
		}
	}
	return c.handleStreamResponse(ctx, req, resp, onMessage, logger)
}

func (c *Client) handleStreamResponse(ctx context.Context, req upstream.UpstreamRequest, resp *http.Response, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if resp == nil {
		return fmt.Errorf("warp stream response is nil")
	}
	if onMessage == nil {
		onMessage = func(upstream.SSEMessage) {}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := readLimitedBody(resp, 4096)
		_ = resp.Body.Close()
		bodyText := strings.TrimSpace(string(body))
		location := ""
		if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != warpAIURL {
			location = resp.Request.URL.String()
		}
		if headerLocation := strings.TrimSpace(resp.Header.Get("Location")); headerLocation != "" {
			location = headerLocation
		}
		if logger != nil {
			logger.LogUpstreamHTTPError(warpAIURL, resp.StatusCode, bodyText, nil)
		}
		op := "stream request"
		if location != "" {
			op = fmt.Sprintf("%s redirect=%s", op, location)
		}
		return &HTTPStatusError{
			Operation:  op,
			StatusCode: resp.StatusCode,
			ErrorCode:  resp.Header.Get("X-Warp-Error-Code"),
			RetryAfter: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
			Body:       bodyText,
		}
	}

	if req.ChatSessionID != "" {
		onMessage(upstream.SSEMessage{
			Type:  "model.conversation_id",
			Event: map[string]interface{}{"id": req.ChatSessionID},
		})
	}

	var body io.ReadCloser = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			return err
		}
		defer gr.Close()
		body = gr
	}
	defer resp.Body.Close()

	return processStreamBody(ctx, body, onMessage, logger)
}

func (c *Client) RefreshAccount(ctx context.Context) (string, error) {
	return c.refreshAccount(ctx, false)
}

func (c *Client) ForceRefreshAccount(ctx context.Context) (string, error) {
	return c.refreshAccount(ctx, true)
}

func (c *Client) refreshAccount(ctx context.Context, force bool) (string, error) {
	if c == nil || c.session == nil {
		return "", fmt.Errorf("warp session not initialized")
	}
	ctx, cancel := util.WithDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	if force {
		c.session.clearToken()
	}
	if _, err := c.ensureAuthenticated(ctx, false); err != nil {
		return "", err
	}
	return c.session.currentJWT(), nil
}

func (c *Client) SyncAccountState() bool {
	if c == nil || c.account == nil || c.session == nil {
		return false
	}

	refresh := c.session.currentRefreshToken()

	changed := false
	// These fields were legacy Warp credential inputs. Clear them whenever the
	// account is synchronized so persisted records converge on refresh_token.
	if c.account.Token != "" {
		c.account.Token = ""
		changed = true
	}
	if c.account.ClientCookie != "" {
		c.account.ClientCookie = ""
		changed = true
	}
	if c.account.SessionCookie != "" {
		c.account.SessionCookie = ""
		changed = true
	}
	if refresh != "" && refresh != c.account.RefreshToken {
		c.account.RefreshToken = refresh
		changed = true
	}
	if deviceID := c.session.currentDeviceID(); deviceID != "" && deviceID != c.account.DeviceID {
		c.account.DeviceID = deviceID
		changed = true
	}
	if requestID := c.session.currentRequestID(); requestID != "" && requestID != c.account.RequestID {
		c.account.RequestID = requestID
		changed = true
	}
	return changed
}

func (c *Client) requestTimeout() time.Duration {
	timeout := defaultRequestTimeout
	if c != nil && c.config != nil && c.config.RequestTimeout > 0 {
		timeout = time.Duration(c.config.RequestTimeout) * time.Second
	}
	if timeout > maxRequestTimeout {
		return maxRequestTimeout
	}
	return timeout
}

func (c *Client) ensureAuthenticated(ctx context.Context, login bool) (*http.Client, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("warp session not initialized")
	}
	authClient := c.authClient
	if authClient == nil {
		authClient = c.httpClient
	}
	if err := c.session.ensureToken(ctx, authClient); err != nil {
		return nil, err
	}
	if login {
		if err := c.session.ensureLogin(ctx, c.httpClient); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			slog.Warn("Warp login notification failed; continuing with authenticated request", "error", err)
		}
	}
	return authClient, nil
}
