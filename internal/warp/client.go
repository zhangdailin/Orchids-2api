package warp

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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

const defaultRequestTimeout = 300 * time.Second

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	if acc == nil {
		httpClient := newHTTPClient(0, cfg)
		return &Client{config: cfg, httpClient: httpClient}
	}

	refresh := ResolveRefreshToken(acc)
	sess := getSession(acc.ID, refresh, acc.DeviceID, acc.RequestID)
	if token := strings.TrimSpace(acc.Token); token != "" {
		sess.seedJWT(token)
	}
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
	}

	var proxyFunc func(*http.Request) (*url.URL, error)
	if cfg != nil {
		proxyFunc = util.ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
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

func (c *Client) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	req := upstream.UpstreamRequest{
		Prompt:      prompt,
		ChatHistory: chatHistory,
		Model:       model,
	}
	return c.SendRequestWithPayload(ctx, req, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("warp session not initialized")
	}

	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	authClient := c.authHTTPClient()
	if err := c.session.ensureToken(ctx, authClient); err != nil {
		return err
	}
	if err := c.session.ensureLogin(ctx, c.httpClient); err != nil {
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
		if err := c.session.ensureToken(ctx, authClient); err != nil {
			return err
		}
		return c.session.ensureLogin(ctx, c.httpClient)
	}
	return c.streamWithRetry(ctx, payload, req, onMessage, logger, defaultRefresh)
}

func (c *Client) doStreamRequest(ctx context.Context, payload []byte, logger *debug.Logger) (*http.Response, error) {
	jwt := c.session.currentJWT()
	if jwt == "" {
		return nil, fmt.Errorf("warp jwt missing")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, warpLegacyAIURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-Warp-Client-ID", clientID)
	req.Header.Set("X-Warp-Client-Version", clientVersion)
	req.Header.Set("X-Warp-OS-Category", clientOSCategory)
	req.Header.Set("X-Warp-OS-Name", clientOSName)
	req.Header.Set("X-Warp-OS-Version", clientOSVersion)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(payload)))
	req.Header.Set("User-Agent", "")

	if logger != nil {
		headers := map[string]string{}
		for k, v := range req.Header {
			headers[k] = strings.Join(v, ", ")
		}
		logger.LogUpstreamRequest(warpLegacyAIURL, headers, payload)
	}

	return c.httpClient.Do(req)
}

func (c *Client) streamWithRetry(ctx context.Context, payload []byte, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger, refresh func() error) error {
	c.session.beginRequest()

	resp, err := c.doStreamRequest(ctx, payload, logger)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		bodyText := strings.TrimSpace(string(body))
		location := ""
		if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != warpLegacyAIURL {
			location = resp.Request.URL.String()
		}
		if headerLocation := strings.TrimSpace(resp.Header.Get("Location")); headerLocation != "" {
			location = headerLocation
		}
		if logger != nil {
			logger.LogUpstreamHTTPError(warpLegacyAIURL, resp.StatusCode, bodyText, nil)
		}
		op := "stream request"
		if location != "" {
			op = fmt.Sprintf("%s redirect=%s", op, location)
		}
		return &HTTPStatusError{
			Operation:  op,
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()),
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
	if c == nil || c.session == nil {
		return "", fmt.Errorf("warp session not initialized")
	}
	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	if err := c.session.ensureToken(ctx, c.authHTTPClient()); err != nil {
		return "", err
	}
	return c.session.currentJWT(), nil
}


func (c *Client) ForceRefreshAccount(ctx context.Context) (string, error) {
	if c == nil || c.session == nil {
		return "", fmt.Errorf("warp session not initialized")
	}
	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	c.session.clearToken()
	if err := c.session.ensureToken(ctx, c.authHTTPClient()); err != nil {
		return "", err
	}
	return c.session.currentJWT(), nil
}

func (c *Client) SyncAccountState() bool {
	if c == nil || c.account == nil || c.session == nil {
		return false
	}

	jwt := c.session.currentJWT()
	refresh := c.session.currentRefreshToken()

	changed := false
	if jwt != "" && jwt != c.account.Token {
		c.account.Token = jwt
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
	if c != nil && c.config != nil && c.config.RequestTimeout > 0 {
		return time.Duration(c.config.RequestTimeout) * time.Second
	}
	return defaultRequestTimeout
}

func (c *Client) authHTTPClient() *http.Client {
	if c != nil && c.authClient != nil {
		return c.authClient
	}
	if c != nil {
		return c.httpClient
	}
	return nil
}

func withDefaultTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
