package orchids

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/clerk"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

var orchidsAgentModelMap = map[string]string{
	"claude-sonnet-4-5":          "claude-sonnet-4-6",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4-6",
	"claude-opus-4-6":            "claude-opus-4-6",
	"claude-opus-4-5":            "claude-opus-4-6",
	"claude-opus-4-5-thinking":   "claude-opus-4-5-thinking",
	"claude-opus-4-6-thinking":   "claude-opus-4-6",
	"claude-haiku-4-5":           "claude-haiku-4-5",
	"claude-sonnet-4-20250514":   "claude-sonnet-4-20250514",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet-20250219",
	"gemini-3-flash":             "gemini-3-flash",
	"gemini-3-pro":               "gemini-3-pro",
	"gpt-5.3-codex":              "gpt-5.3-codex",
	"gpt-5.2-codex":              "gpt-5.2-codex",
	"gpt-5.2":                    "gpt-5.2",
	"grok-4.1-fast":              "grok-4.1-fast",
	"glm-5":                      "glm-5",
	"kimi-k2.5":                  "kimi-k2.5",
}

const orchidsAgentDefaultModel = "claude-sonnet-4-6"

const (
	orchidsWSConnectTimeout = 5 * time.Second
	orchidsWSReadTimeout    = 600 * time.Second
	orchidsWSRequestTimeout = 60 * time.Second
	orchidsWSPingInterval   = 10 * time.Second
	orchidsWSUserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36"
	orchidsWSOrigin         = "https://www.orchids.app"
)

type wsFallbackError struct {
	err error
}

func (e wsFallbackError) Error() string {
	return e.err.Error()
}

func (e wsFallbackError) Unwrap() error {
	return e.err
}

// Orchids Event Types
const (
	EventConnected          = "connected"
	EventCodingAgentStart   = "coding_agent.start"
	EventCodingAgentInit    = "coding_agent.initializing"
	EventCodingAgentTokens  = "coding_agent.tokens_used"
	EventCreditsExhausted   = "coding_agent.credits_exhausted"
	EventResponseDone       = "response_done"
	EventCodingAgentEnd     = "coding_agent.end"
	EventComplete           = "complete"
	EventTodoWriteStart     = "coding_agent.todo_write.started"
	EventRunItemStream      = "run_item_stream_event"
	EventToolCallOutput     = "tool_call_output_item"
	EventEditStart          = "coding_agent.Edit.edit.started"
	EventEditChunk          = "coding_agent.Edit.edit.chunk"
	EventEditFileCompleted  = "coding_agent.edit_file.completed"
	EventEditCompleted      = "coding_agent.Edit.edit.completed"
	EventWriteStart         = "coding_agent.Write.started"
	EventWriteContentStart  = "coding_agent.Write.content.started"
	EventWriteChunk         = "coding_agent.Write.content.chunk"
	EventWriteCompleted     = "coding_agent.Write.content.completed"
	EventReasoningChunk     = "coding_agent.reasoning.chunk"
	EventReasoningCompleted = "coding_agent.reasoning.completed"
	EventOutputTextDelta    = "output_text_delta"
	EventResponseChunk      = "coding_agent.response.chunk"
	EventModel              = "model"
	EventResponseStarted    = "response_started"
)

func (c *Client) getWSToken() (string, error) {
	if c.config != nil && strings.TrimSpace(c.config.UpstreamToken) != "" {
		return c.config.UpstreamToken, nil
	}

	if c.config != nil && strings.TrimSpace(c.config.ClientCookie) != "" {
		proxyFunc := http.ProxyFromEnvironment
		if c.config != nil {
			proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
		}
		info, err := clerk.FetchAccountInfoWithProjectAndSessionProxy(c.config.ClientCookie, c.config.SessionCookie, c.config.ProjectID, proxyFunc)
		if err == nil && info.JWT != "" {
			return info.JWT, nil
		}
	}

	return c.GetToken()
}

func (c *Client) dialWSConnection(ctx context.Context) (*websocket.Conn, error) {
	if c.config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	token, err := c.getWSToken()
	if err != nil {
		return nil, fmt.Errorf("failed to get ws token: %w", err)
	}

	wsURL := c.buildWSURL(token)
	if wsURL == "" {
		return nil, fmt.Errorf("ws url not configured")
	}

	headers := http.Header{
		"User-Agent": []string{orchidsWSUserAgent},
		"Origin":     []string{orchidsWSOrigin},
	}

	proxyFunc := http.ProxyFromEnvironment
	if c.config != nil {
		proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: orchidsWSConnectTimeout,
		Proxy:            proxyFunc,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, fmt.Errorf("failed to dial WebSocket: %w", err)
	}

	return conn, nil
}

func (c *Client) sendRequestWS(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	slog.Debug("sendRequestWS called", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "workdir", req.Workdir, "model", req.Model)
	parentCtx := ctx
	chatSessionID := orchidsChatSessionID(req)
	timeout := orchidsWSRequestTimeout
	if c.config != nil && c.config.RequestTimeout > 0 {
		timeout = time.Duration(c.config.RequestTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startPool := time.Now()

	ws := GetConnectionPoolManager().GetConnection(c.wsConnectionKey(), c.dialWSConnection)
	if err := ws.BeginRequest(ctx); err != nil {
		if parentCtx.Err() == nil {
			return wsFallbackError{err: fmt.Errorf("ws dial failed: %w", err)}
		}
		return fmt.Errorf("ws dial failed: %w", err)
	}
	defer ws.EndRequest()

	invalidateConn := false
	defer func() {
		if invalidateConn {
			_ = ws.Invalidate()
		}
	}()

	pingDone := make(chan struct{})
	defer close(pingDone)

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS connection acquired", "duration", time.Since(startPool))
	}

	startWrite := time.Now()
	requestWritten := false

	orchidsReq, err := c.buildWSRequest(req)
	if err != nil {
		invalidateConn = true
		return err
	}

	// Note: Logger disabled for pooled connections
	// if logger != nil {
	// 	logger.LogUpstreamRequest(wsURL, logHeaders, wsPayload)
	// }

	if err := ws.SendRequest(orchidsReq); err != nil {
		if parentCtx.Err() == nil {
			invalidateConn = true
			slog.Warn("Orchids WS write failed before first message", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "request_written", requestWritten, "error", err)
			return wsFallbackError{err: fmt.Errorf("ws write failed: %w", err)}
		}
		invalidateConn = true
		return fmt.Errorf("ws write failed: %w", err)
	}
	requestWritten = true
	slog.Info("Orchids WS request sent", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "model", req.Model)

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS WriteJSON completed", "duration", time.Since(startWrite))
	}

	startFirstToken := time.Now()
	firstReceived := false
	receivedAnyMessage := false

	runtime := newOrchidsRequestRuntime(req, onMessage)
	state := &runtime.state

	// Start Keep-Alive Ping Loop
	go func() {
		ticker := time.NewTicker(orchidsWSPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingDone:
				return
			case <-ticker.C:
				if err := ws.Ping(time.Now().Add(10 * time.Second)); err != nil {
					return
				}
			}
		}
	}()

	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Invalidate()
		case <-ctxDone:
		}
	}()
	defer close(ctxDone)

	for {
		if ctx.Err() != nil {
			invalidateConn = true
			return ctx.Err()
		}
		data, err := ws.ReadMessageWithTimeout(orchidsWSReadTimeout)
		if err != nil {
			if ctx.Err() != nil {
				invalidateConn = true
				return ctx.Err()
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				invalidateConn = true
				break
			}
			if parentCtx.Err() == nil && !receivedAnyMessage {
				invalidateConn = true
				slog.Warn("Orchids WS fallback before first message", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "request_written", requestWritten, "stage", "read_message", "error", err)
				return wsFallbackError{err: err}
			}
			invalidateConn = true
			break
		}

		handled, shouldBreak := runtime.handleRawMessage(data, logger, req.Tools)
		if handled {
			receivedAnyMessage = true
			if !firstReceived {
				slog.Info("Orchids WS first upstream message received", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID)
			}
			if !firstReceived && c.config.DebugEnabled {
				firstReceived = true
				slog.Info("[Performance] WS First response received (TTFT)", "duration", time.Since(startFirstToken))
			}
			firstReceived = true
			if shouldBreak {
				break
			}
			continue
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		receivedAnyMessage = true
		if !firstReceived {
			slog.Info("Orchids WS first upstream message received", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID)
		}

		if !firstReceived && c.config.DebugEnabled {
			firstReceived = true
			slog.Info("[Performance] WS First response received (TTFT)", "duration", time.Since(startFirstToken))
		}
		firstReceived = true

		shouldBreak = runtime.handleDecodedMessage(msg, data, logger, req.Tools)
		if shouldBreak {
			break
		}
	}

	if err := runtime.finalize(ctx); err != nil {
		if state.errorMsg != "" {
			slog.Warn("Orchids WS stream ended with upstream error", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "error", state.errorMsg)
		}
		if ctx.Err() != nil {
			invalidateConn = true
		}
		return err
	}

	slog.Info("Orchids WS request completed", "trace_id", traceIDForLog(req), "attempt", attemptForLog(req), "chat_session_id", chatSessionID, "saw_tool_call", state.sawToolCall, "response_started", state.responseStarted)
	return nil
}

func (c *Client) buildWSURL(token string) string {
	if c.config == nil {
		return ""
	}
	wsURL := c.config.OrchidsWSURL
	sep := "?"
	if strings.Contains(wsURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stoken=%s", wsURL, sep, urlEncode(token))
}

func (c *Client) buildWSRequest(req upstream.UpstreamRequest) (*OrchidsRequest, error) {
	if c.config == nil {
		return nil, errors.New("server config unavailable")
	}
	request := buildOrchidsRequest(req, c.config)
	return &request, nil
}

func isSuggestionModeText(text string) bool {
	normalized := strings.ToLower(text)
	return strings.Contains(normalized, "suggestion mode")
}

func normalizeOrchidsAgentModel(model string) string {
	mapped := normalizeOrchidsModelKey(model)
	if mapped == "" {
		return orchidsAgentDefaultModel
	}
	if resolved, ok := orchidsAgentModelMap[mapped]; ok {
		return resolved
	}
	return orchidsAgentDefaultModel
}

func normalizeOrchidsModelKey(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(normalized, "claude-") {
		normalized = strings.ReplaceAll(normalized, "4.6", "4-6")
		normalized = strings.ReplaceAll(normalized, "4.5", "4-5")
	}
	return normalized
}
