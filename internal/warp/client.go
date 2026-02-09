package warp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

type Client struct {
	config     *config.Config
	account    *store.Account
	httpClient *http.Client
	session    *session
}

func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	if acc == nil {
		return &Client{config: cfg}
	}
	refresh := strings.TrimSpace(acc.RefreshToken)
	sess := getSession(acc.ID, refresh)

	// Inject JWT token if available, valid, and session is empty
	if acc.Token != "" {
		sess.mu.Lock()
		if sess.jwt == "" {
			if exp := jwtExpiry(acc.Token); !exp.IsZero() && time.Now().Add(20*time.Minute).Before(exp) {
				sess.jwt = acc.Token
				sess.expiresAt = exp
			}
		}
		sess.mu.Unlock()
	}

	timeout := defaultRequestTimeout
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
	}

	client := newHTTPClient(timeout, cfg)
	// Set jar under session lock to avoid concurrent mutation
	sess.mu.Lock()
	client.Jar = sess.jar
	sess.mu.Unlock()

	return &Client{
		config:     cfg,
		account:    acc,
		httpClient: client,
		session:    sess,
	}
}

const defaultRequestTimeout = 120 * time.Second

func newHTTPClient(timeout time.Duration, cfg *config.Config) *http.Client {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}

	var proxyURL *url.URL
	if cfg != nil && cfg.ProxyHTTP != "" {
		parsedURL, err := url.Parse(cfg.ProxyHTTP)
		if err == nil {
			if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
				parsedURL.User = url.UserPassword(cfg.ProxyUser, cfg.ProxyPass)
			}
			proxyURL = parsedURL
		}
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: newUTLSTransport(proxyURL),
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
	if c.session == nil {
		return fmt.Errorf("warp session not initialized")
	}
	ctx, cancel := withDefaultTimeout(ctx, c.requestTimeout())
	defer cancel()

	cid := clientID
	if c.account != nil && c.account.SessionID != "" {
		cid = c.account.SessionID
	} else if c.account != nil {
		cid = fmt.Sprintf("warp-%d", c.account.ID)
	}

	promptText := req.Prompt
	model := req.Model
	messages := req.Messages
	workdir := req.Workdir
	conversationID := req.ChatSessionID

	if c.config != nil && c.config.DebugEnabled {
		slog.Debug("Warp AI: Preparing request", "cid", cid, "conversationID", conversationID)
	}

	if err := c.session.ensureToken(ctx, c.httpClient, cid); err != nil {
		slog.Warn("Warp AI: ensureToken failed", "error", err)
		return err
	}
	if err := c.session.ensureLogin(ctx, c.httpClient, cid); err != nil {
		slog.Warn("Warp AI: ensureLogin failed", "error", err)
		return err
	}

	rawToolCount := len(req.Tools)
	disableWarpTools := false
	var cfgDisable interface{}
	if c.config != nil && c.config.WarpDisableTools != nil {
		disableWarpTools = *c.config.WarpDisableTools
		cfgDisable = *c.config.WarpDisableTools
	}
	if req.NoTools {
		disableWarpTools = true
	}
	if rawToolCount == 0 {
		disableWarpTools = true
	}
	if c.config != nil && c.config.DebugEnabled {
		slog.Debug("Warp 工具开关诊断", "cid", cid, "cfg_warp_disable_tools", cfgDisable, "req_no_tools", req.NoTools, "req_tool_count", rawToolCount, "disable_warp_tools", disableWarpTools)
	}

	tools := req.Tools
	if req.NoTools {
		tools = nil
	}

	var mcpContext []byte
	var err error
	if !disableWarpTools {
		mcpContext, err = buildMCPContext(tools)
		if err != nil {
			return err
		}
	}

	payload, err := buildRequestBytes(promptText, model, messages, mcpContext, disableWarpTools, workdir, conversationID)
	if err != nil {
		return err
	}

	jwt := c.session.currentJWT()
	if jwt == "" {
		return fmt.Errorf("warp jwt missing")
	}
	// 发送前再次校验 JWT 未过期，防止 ensureToken 和发送之间的竞态
	if exp := jwtExpiry(jwt); !exp.IsZero() && time.Now().Add(1*time.Minute).After(exp) {
		slog.Warn("Warp AI: JWT expired before send, forcing re-refresh", "cid", cid, "exp", exp)
		// 清除过期 token 强制重新 refresh
		c.session.mu.Lock()
		c.session.jwt = ""
		c.session.expiresAt = time.Time{}
		c.session.mu.Unlock()
		if err := c.session.ensureToken(ctx, c.httpClient, cid); err != nil {
			return fmt.Errorf("re-refresh after expired JWT: %w", err)
		}
		jwt = c.session.currentJWT()
		if jwt == "" {
			return fmt.Errorf("warp jwt missing after re-refresh")
		}
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
	// 避免 Go 默认注入 User-Agent
	request.Header.Set("user-agent", "")

	if logger != nil {
		headers := make(map[string]string)
		for k, v := range request.Header {
			headers[k] = strings.Join(v, ", ")
		}
		logger.LogUpstreamRequest(aiURL, headers, payload)
	}

	breaker := upstream.GetAccountBreaker(c.breakerKey())
	start := time.Now()

	if c.config != nil && c.config.DebugEnabled {
		reqHeaders := make(map[string]string)
		for k, v := range request.Header {
			reqHeaders[k] = strings.Join(v, ", ")
		}
		slog.Debug("Warp AI: Dispatching request", "url", aiURL, "headers", reqHeaders, "body_size", len(payload))
	}

	result, err := breaker.Execute(func() (interface{}, error) {
		return c.httpClient.Do(request)
	})
	if err != nil {
		if c.config != nil && c.config.DebugEnabled {
			slog.Info("Warp AI: Request Failed", "duration", time.Since(start), "error", err)
		}
		return err
	}
	if c.config != nil && c.config.DebugEnabled {
		slog.Debug("Warp AI: Response Headers Received", "duration", time.Since(start))
	}
	resp, ok := result.(*http.Response)
	if !ok || resp == nil {
		return fmt.Errorf("warp api error: unexpected response type")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if logger != nil {
			logger.LogUpstreamHTTPError(aiURL, resp.StatusCode, string(body), nil)
		}
		headerLog := make(map[string]string)
		for k, v := range resp.Header {
			headerLog[k] = strings.Join(v, ", ")
		}
		slog.Warn("Warp AI request failed", "status", resp.StatusCode, "headers", headerLog, "body", string(body))
		return fmt.Errorf("warp api error: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var reader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		var err error
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer reader.Close()
	}

	bufReader := bufio.NewReader(reader)
	var dataLines []string
	dataEventCount := 0
	parsedEventCount := 0
	toolCallSeen := false
	finishSent := false
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = resp.Body.Close()
		case <-ctxDone:
		}
	}()
	defer close(ctxDone)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := bufReader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
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
			dataEventCount++
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
			parsedEventCount++
			if parsed.ConversationID != "" {
				onMessage(upstream.SSEMessage{Type: "model.conversation_id", Event: map[string]interface{}{"id": parsed.ConversationID}})
			}
			if parsed.Error != "" {
				slog.Warn("Warp upstream error in stream", "error", parsed.Error)
				return fmt.Errorf("warp stream error: %s", parsed.Error)
			}
			for _, delta := range parsed.TextDeltas {
				onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": delta}})
			}
			for _, delta := range parsed.ReasoningDeltas {
				onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{"delta": delta}})
			}
			for _, call := range parsed.ToolCalls {
				toolCallSeen = true
				onMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{"toolCallId": call.ID, "toolName": call.Name, "input": call.Input}})
			}
			if parsed.Finish != nil {
				finishSent = true
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
				onMessage(upstream.SSEMessage{Type: "model.finish", Event: finish})
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
	if dataEventCount == 0 {
		if logger != nil {
			logger.LogUpstreamSSE("warp_empty_stream", "stream ended without any SSE data events")
		}
		return fmt.Errorf("warp stream ended without any SSE data events")
	}
	if parsedEventCount == 0 {
		if logger != nil {
			logger.LogUpstreamSSE("warp_unparsed_stream", fmt.Sprintf("received %d SSE data events but none parsed", dataEventCount))
		}
		return fmt.Errorf("warp stream received %d SSE data events but none parsed", dataEventCount)
	}

	if !finishSent {
		if !toolCallSeen {
			onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "end_turn"}})
		} else {
			onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}})
		}
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
	cid := clientID
	if c.account != nil && c.account.SessionID != "" {
		cid = c.account.SessionID
	} else if c.account != nil {
		cid = fmt.Sprintf("warp-%d", c.account.ID)
	}
	if err := c.session.ensureToken(ctx, c.httpClient, cid); err != nil {
		return "", err
	}
	jwt := c.session.currentJWT()
	if jwt == "" {
		return "", fmt.Errorf("warp jwt missing")
	}
	return jwt, nil
}

// SyncAccountState 同步内存会话中的 JWT 和 refresh_token 到账号信息，返回是否有变更。
func (c *Client) SyncAccountState() bool {
	if c == nil || c.session == nil || c.account == nil {
		return false
	}
	changed := false
	jwt := strings.TrimSpace(c.session.currentJWT())
	if jwt != "" && jwt != c.account.Token {
		c.account.Token = jwt
		changed = true
	}
	// 同步 refresh_token，防止服务重启后使用已轮换的旧令牌
	newRefresh := strings.TrimSpace(c.session.currentRefreshToken())
	if newRefresh != "" && newRefresh != c.account.RefreshToken {
		c.account.RefreshToken = newRefresh
		changed = true
	}
	return changed
}

// jwtExpiry parses the exp claim from a JWT token and returns the expiry time.
func jwtExpiry(token string) time.Time {
	firstDot := strings.IndexByte(token, '.')
	if firstDot < 0 {
		return time.Time{}
	}
	rest := token[firstDot+1:]
	secondDot := strings.IndexByte(rest, '.')
	if secondDot < 0 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(rest[:secondDot])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0)
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
