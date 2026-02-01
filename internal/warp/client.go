package warp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"
	"orchids-api/internal/upstream"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
	session    *session
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	refresh := ""
	if acc != nil {
		refresh = strings.TrimSpace(acc.ClientCookie)
	}
	sess := getSession(acc.ID, refresh)
	timeout := defaultRequestTimeout
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}
	return &Client{
		config:     cfg,
		account:    acc,
		httpClient: newHTTPClient(timeout),
		session:    sess,
	}
}

const defaultRequestTimeout = 120 * time.Second

func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func (c *Client) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(orchids.SSEMessage), logger *debug.Logger) error {
	req := orchids.UpstreamRequest{
		Prompt:      prompt,
		ChatHistory: chatHistory,
		Model:       model,
	}
	return c.SendRequestWithPayload(ctx, req, onMessage, logger)
}

func (c *Client) SendRequestWithPayload(ctx context.Context, req orchids.UpstreamRequest, onMessage func(orchids.SSEMessage), logger *debug.Logger) error {
	if c.session == nil {
		return fmt.Errorf("warp session not initialized")
	}
	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	if err := c.session.ensureToken(ctx, c.httpClient); err != nil {
		return err
	}
	if err := c.session.ensureLogin(ctx, c.httpClient); err != nil {
		return err
	}

	prompt := req.Prompt
	hasHistory := len(req.Messages) > 1
	disableWarpTools := true
	if c.config != nil && c.config.WarpDisableTools != nil {
		disableWarpTools = *c.config.WarpDisableTools
	}
	if req.NoTools {
		disableWarpTools = true
	}

	tools := req.Tools
	if req.NoTools {
		tools = nil
	}

	payload, err := buildRequestBytes(prompt, req.Model, tools, disableWarpTools, hasHistory, req.Workdir)
	if err != nil {
		return err
	}

	jwt := c.session.currentJWT()
	if jwt == "" {
		return fmt.Errorf("warp jwt missing")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, aiURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("x-warp-client-id", clientID)
	request.Header.Set("accept", "text/event-stream")
	request.Header.Set("content-type", "application/x-protobuf")
	request.Header.Set("x-warp-client-version", clientVersion)
	request.Header.Set("x-warp-os-category", osCategory)
	request.Header.Set("x-warp-os-name", osName)
	request.Header.Set("x-warp-os-version", osVersion)
	request.Header.Set("authorization", "Bearer "+jwt)
	request.Header.Set("accept-encoding", "identity")
	request.Header.Set("content-length", fmt.Sprintf("%d", len(payload)))

	if logger != nil {
		logger.LogUpstreamRequest(aiURL, map[string]string{
			"Accept":                "text/event-stream",
			"Authorization":         "Bearer [REDACTED]",
			"Content-Type":          "application/x-protobuf",
			"X-Warp-Client-Version": clientVersion,
		}, map[string]interface{}{"payload_bytes": len(payload)})
	}

	breaker := upstream.GetAccountBreaker(c.breakerKey())
	start := time.Now()

	result, err := breaker.Execute(func() (interface{}, error) {
		return c.httpClient.Do(request)
	})
	if err != nil {
		if c.config != nil && c.config.DebugEnabled {
			slog.Info("[Performance] Upstream Request Failed", "duration", time.Since(start), "error", err)
		}
		return err
	}
	if c.config != nil && c.config.DebugEnabled {
		slog.Info("[Performance] Upstream Request Success", "duration", time.Since(start))
	}
	resp, ok := result.(*http.Response)
	if !ok || resp == nil {
		return fmt.Errorf("warp api error: unexpected response type")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("warp api error: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	toolCallSeen := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) == 0 {
				continue
			}
			data := strings.Join(dataLines, "")
			dataLines = nil
			if logger != nil {
				logger.LogUpstreamSSE("warp_data", data)
			}
			payloadBytes, err := decodeWarpPayload(data)
			if err != nil {
				if logger != nil {
					logger.LogUpstreamSSE("warp_decode_error", err.Error())
				}
				continue
			}
			parsed, err := parseResponseEvent(payloadBytes)
			if err != nil {
				if logger != nil {
					logger.LogUpstreamSSE("warp_parse_error", err.Error())
				}
				continue
			}
		for _, delta := range parsed.TextDeltas {
			onMessage(orchids.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": delta}})
		}
		for _, delta := range parsed.ReasoningDeltas {
			onMessage(orchids.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{"delta": delta}})
		}
		for _, call := range parsed.ToolCalls {
			toolCallSeen = true
			onMessage(orchids.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{"toolCallId": call.ID, "toolName": call.Name, "input": call.Input}})
		}
		if parsed.Finish != nil {
			finish := map[string]interface{}{
				"finishReason": "end_turn",
			}
			if toolCallSeen {
				finish["finishReason"] = "tool_use"
			}
			if parsed.Finish.InputTokens > 0 || parsed.Finish.OutputTokens > 0 {
				finish["usage"] = map[string]interface{}{
					"inputTokens":  parsed.Finish.InputTokens,
					"outputTokens": parsed.Finish.OutputTokens,
				}
			}
			onMessage(orchids.SSEMessage{Type: "model.finish", Event: finish})
		}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[5:]))
			continue
		}
		// ignore event: or other lines
	}

	// Send finish if stream ended without explicit finish event
	if !toolCallSeen {
		onMessage(orchids.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "end_turn"}})
	} else {
		onMessage(orchids.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}})
	}

	return nil
}

func (c *Client) requestTimeout() time.Duration {
	if c != nil && c.config != nil && c.config.RequestTimeout > 0 {
		return time.Duration(c.config.RequestTimeout) * time.Second
	}
	return defaultRequestTimeout
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

func (c *Client) breakerKey() string {
	if c == nil || c.account == nil {
		return "warp:default"
	}
	if name := strings.TrimSpace(c.account.Name); name != "" {
		return "warp:" + name
	}
	if c.account.ID > 0 {
		return fmt.Sprintf("warp:%d", c.account.ID)
	}
	return "warp:default"
}

func decodeWarpPayload(data string) ([]byte, error) {
	if data == "" {
		return nil, fmt.Errorf("empty payload")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(data); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(data)
}

func (c *Client) RefreshAccount(ctx context.Context) (string, error) {
	if c.session == nil {
		return "", fmt.Errorf("warp session not initialized")
	}
	if err := c.session.refreshTokenRequest(ctx, c.httpClient); err != nil {
		return "", err
	}
	if c.account != nil {
		newRefresh := c.session.currentRefreshToken()
		if newRefresh != "" {
			c.account.ClientCookie = newRefresh
		}
	}
	jwt := c.session.currentJWT()
	if jwt == "" {
		return "", fmt.Errorf("warp jwt missing")
	}
	return jwt, nil
}

// SyncAccountState 同步内存会话中的刷新令牌与 JWT 到账号信息，返回是否有变更。
func (c *Client) SyncAccountState() bool {
	if c == nil || c.session == nil || c.account == nil {
		return false
	}
	changed := false
	jwt := strings.TrimSpace(c.session.currentJWT())
	refresh := strings.TrimSpace(c.session.currentRefreshToken())
	if refresh != "" && refresh != c.account.ClientCookie {
		c.account.ClientCookie = refresh
		changed = true
	}
	if jwt != "" && jwt != c.account.Token {
		c.account.Token = jwt
		changed = true
	}
	return changed
}

func (c *Client) LogSessionState() {
	if c.session == nil {
		return
	}
	jwt := c.session.currentJWT()
	if jwt == "" {
		return
	}
	slog.Debug("warp session ready")
}
