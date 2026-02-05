package orchids

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

var orchidsAIClientModels = []string{
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"claude-haiku-4-5",
	"gemini-3-flash",
	"gpt-5.2",
}

const orchidsAIClientDefaultModel = "claude-sonnet-4-5"

// Orchids Event Types
const (
	EventConnected          = "connected"
	EventCodingAgentStart   = "coding_agent.start"
	EventCodingAgentInit    = "coding_agent.initializing"
	EventCodingAgentTokens  = "coding_agent.tokens_used"
	EventResponseDone       = "response_done"
	EventCodingAgentEnd     = "coding_agent.end"
	EventComplete           = "complete"
	EventFS                 = "fs_operation"
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

type requestState struct {
	preferCodingAgent bool
	textStarted       bool
	reasoningStarted  bool
	lastTextDelta     string
	finishSent        bool
	sawToolCall       bool
	hasFSOps          bool
	responseStarted   bool
	suppressStarts    bool
	activeWrites      map[string]*fileWriterState
}

type fileWriterState struct {
	path string
	buf  strings.Builder
}

func (c *Client) sendRequestWSAIClient(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	slog.Info("sendRequestWSAIClient called", "workdir", req.Workdir, "model", req.Model)
	parentCtx := ctx
	timeout := orchidsWSRequestTimeout
	if c.config != nil && c.config.RequestTimeout > 0 {
		timeout = time.Duration(c.config.RequestTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startPool := time.Now()

	// Get connection from pool (or create new if pool unavailable)
	var conn *websocket.Conn
	var err error
	var returnToPool bool
	var pingDone chan struct{}

	if c.wsPool != nil {
		conn, err = c.wsPool.Get(ctx)
		if err != nil {
			// Fall back to direct connection if pool fails
			token, err := c.getWSToken()
			if err != nil {
				return fmt.Errorf("failed to get ws token: %w", err)
			}
			wsURL := c.buildWSURLAIClient(token)
			if wsURL == "" {
				return errors.New("ws url not configured")
			}
			headers := http.Header{
				"User-Agent": []string{orchidsWSUserAgent},
				"Origin":     []string{orchidsWSOrigin},
			}
			dialer := websocket.Dialer{
				HandshakeTimeout: orchidsWSConnectTimeout,
			}
			conn, _, err = dialer.DialContext(ctx, wsURL, headers)
			if err != nil {
				if parentCtx.Err() == nil {
					return wsFallbackError{err: fmt.Errorf("ws dial failed: %w", err)}
				}
				return fmt.Errorf("ws dial failed: %w", err)
			}
			defer conn.Close()
		} else {
			// Successfully got connection from pool
			// Return to pool when done (unless error occurs)
			returnToPool = true
			pingDone = make(chan struct{})
			defer func() {
				close(pingDone)
				if conn == nil {
					return
				}
				if returnToPool {
					c.wsPool.Put(conn)
				} else {
					conn.Close()
				}
			}()
		}
	} else {
		// No pool available, create connection directly
		token, err := c.getWSToken()
		if err != nil {
			return fmt.Errorf("failed to get ws token: %w", err)
		}
		wsURL := c.buildWSURLAIClient(token)
		if wsURL == "" {
			return errors.New("ws url not configured")
		}
		headers := http.Header{
			"User-Agent": []string{orchidsWSUserAgent},
			"Origin":     []string{orchidsWSOrigin},
		}
		dialer := websocket.Dialer{
			HandshakeTimeout: orchidsWSConnectTimeout,
		}
		conn, _, err = dialer.DialContext(ctx, wsURL, headers)
		if err != nil {
			if parentCtx.Err() == nil {
				return wsFallbackError{err: fmt.Errorf("ws dial failed: %w", err)}
			}
			return fmt.Errorf("ws dial failed: %w", err)
		}
		defer conn.Close()
	}

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS connection acquired", "duration", time.Since(startPool))
	}

	startWrite := time.Now()

	wsPayload, err := c.buildWSRequestAIClient(req)
	if err != nil {
		returnToPool = false
		return err
	}

	// Note: Logger disabled for pooled connections
	// if logger != nil {
	// 	logger.LogUpstreamRequest(wsURL, logHeaders, wsPayload)
	// }

	// Lock to prevent race with ping loop which starts shortly after
	c.wsWriteMu.Lock()
	writeErr := conn.WriteJSON(wsPayload)
	c.wsWriteMu.Unlock()

	if writeErr != nil {
		if parentCtx.Err() == nil {
			returnToPool = false
			return wsFallbackError{err: fmt.Errorf("ws write failed: %w", writeErr)}
		}
		returnToPool = false
		return fmt.Errorf("ws write failed: %w", writeErr)
	}

	if c.config.DebugEnabled {
		slog.Info("[Performance] WS WriteJSON completed", "duration", time.Since(startWrite))
	}

	startFirstToken := time.Now()
	firstReceived := false

	var state requestState
	var fsWG sync.WaitGroup

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
				c.wsWriteMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
				c.wsWriteMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		case <-ctxDone:
		}
	}()
	defer close(ctxDone)

	for {
		if ctx.Err() != nil {
			returnToPool = false
			return ctx.Err()
		}
		if err := conn.SetReadDeadline(time.Now().Add(orchidsWSReadTimeout)); err != nil {
			if ctx.Err() != nil {
				returnToPool = false
				return ctx.Err()
			}
			if parentCtx.Err() == nil {
				returnToPool = false
				return wsFallbackError{err: err}
			}
			returnToPool = false
			return err
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				returnToPool = false
				return ctx.Err()
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				returnToPool = false
				break
			}
			if parentCtx.Err() == nil {
				returnToPool = false
				return wsFallbackError{err: err}
			}
			returnToPool = false
			break
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		if !firstReceived && c.config.DebugEnabled {
			firstReceived = true
			slog.Info("[Performance] WS First response received (TTFT)", "duration", time.Since(startFirstToken))
		}

		shouldBreak := c.handleOrchidsMessage(msg, data, &state, onMessage, logger, conn, &fsWG, req.Workdir)
		if shouldBreak {
			break
		}
	}

	if !state.finishSent {
		finishReason := "stop"
		if state.sawToolCall {
			finishReason = "tool-calls"
		}
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": finishReason}})
	}

	if state.hasFSOps {
		fsDone := make(chan struct{})
		go func() {
			fsWG.Wait()
			close(fsDone)
		}()
		select {
		case <-fsDone:
		case <-ctx.Done():
			returnToPool = false
		}
	}

	return nil
}

func (c *Client) handleOrchidsMessage(
	msg map[string]interface{},
	rawData []byte,
	state *requestState,
	onMessage func(upstream.SSEMessage),
	logger *debug.Logger,
	conn *websocket.Conn,
	fsWG *sync.WaitGroup,
	workdir string,
) bool {
	msgType, _ := msg["type"].(string)
	if logger != nil {
		logger.LogUpstreamSSE(msgType, string(rawData))
	}

	switch msgType {
	case EventConnected:
		return false

	case EventResponseStarted:
		if state.responseStarted {
			state.suppressStarts = true
			return false
		}
		state.responseStarted = true
		return false

	case EventCodingAgentStart, EventCodingAgentInit:
		if state.suppressStarts {
			return false
		}
		state.preferCodingAgent = true
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg, Raw: msg})
		return false

	case EventCodingAgentTokens:
		c.handleTokensEvent(msg, onMessage)
		return false

	case EventResponseDone, EventCodingAgentEnd, EventComplete:
		return c.handleCompletionEvent(msgType, msg, state, onMessage)

	case EventFS:
		c.dispatchFSOperation(msg, onMessage, conn, fsWG, workdir)
		state.hasFSOps = true
		return false

	case EventReasoningChunk:
		state.preferCodingAgent = true
		text := extractOrchidsText(msg)
		if text == "" {
			return false
		}
		if !state.reasoningStarted {
			state.reasoningStarted = true
			onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-start", "id": "0"}})
		}
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-delta", "id": "0", "delta": text}})
		return false

	case EventReasoningCompleted:
		state.preferCodingAgent = true
		if state.reasoningStarted {
			onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-end", "id": "0"}})
			state.reasoningStarted = false // Reset for safety, though likely end of turn
		}
		return false

	case EventOutputTextDelta, EventResponseChunk:
		state.preferCodingAgent = true
		text := extractOrchidsText(msg)
		if text == "" {
			return false
		}
		if text == state.lastTextDelta {
			return false
		}
		state.lastTextDelta = text
		if !state.textStarted {
			state.textStarted = true
			onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-start", "id": "0"}})
		}
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-delta", "id": "0", "delta": text}})
		return false

	case EventWriteStart, EventWriteContentStart, EventEditStart:
		state.preferCodingAgent = true
		data, _ := msg["data"].(map[string]interface{})
		path, _ := data["file_path"].(string)
		if path != "" {
			if state.activeWrites == nil {
				state.activeWrites = make(map[string]*fileWriterState)
			}
			state.activeWrites[path] = &fileWriterState{path: path}
		}
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg})
		return false

	case EventWriteChunk, EventEditChunk:
		state.preferCodingAgent = true
		data, _ := msg["data"].(map[string]interface{})
		path, _ := data["file_path"].(string)
		text, _ := data["text"].(string)
		if path != "" && text != "" && state.activeWrites != nil {
			if w, ok := state.activeWrites[path]; ok {
				w.buf.WriteString(text)
			}
		}
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg})
		return false

	case EventWriteCompleted:
		state.preferCodingAgent = true
		data, _ := msg["data"].(map[string]interface{})
		path, _ := data["file_path"].(string)
		if path != "" && state.activeWrites != nil {
			if w, ok := state.activeWrites[path]; ok {
				content := w.buf.String()
				c.dispatchFSOperation(map[string]interface{}{
					"operation": "write",
					"path":      path,
					"content":   content,
					"id":        fmt.Sprintf("stream_%d", time.Now().UnixMilli()),
				}, onMessage, conn, fsWG, workdir)
				delete(state.activeWrites, path)
				state.hasFSOps = true
			}
		}
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg, Raw: msg})
		return false

	case EventEditCompleted, EventEditFileCompleted:
		state.preferCodingAgent = true
		if state.activeWrites != nil {
			data, _ := msg["data"].(map[string]interface{})
			path, _ := data["file_path"].(string)
			if path != "" {
				delete(state.activeWrites, path)
			}
		}
		onMessage(upstream.SSEMessage{Type: msgType, Event: msg, Raw: msg})
		return false

	case EventModel:
		return c.handleModelEvent(msg, state, onMessage)

	// Suppressed events
	case EventTodoWriteStart, EventRunItemStream, EventToolCallOutput:
		return false
	}

	return false
}

func (c *Client) handleTokensEvent(msg map[string]interface{}, onMessage func(upstream.SSEMessage)) {
	data, _ := msg["data"].(map[string]interface{})
	if data == nil {
		return
	}
	event := map[string]interface{}{"type": "tokens-used"}
	if v, ok := data["input_tokens"]; ok {
		event["inputTokens"] = v
	} else if v, ok := data["inputTokens"]; ok {
		event["inputTokens"] = v
	}
	if v, ok := data["output_tokens"]; ok {
		event["outputTokens"] = v
	} else if v, ok := data["outputTokens"]; ok {
		event["outputTokens"] = v
	}
	onMessage(upstream.SSEMessage{Type: "model", Event: event})
}

func (c *Client) handleCompletionEvent(
	msgType string,
	msg map[string]interface{},
	state *requestState,
	onMessage func(upstream.SSEMessage),
) bool {
	if msgType == EventResponseDone {
		// Handle usage
		if usage, ok := msg["response"].(map[string]interface{}); ok {
			if u, ok := usage["usage"].(map[string]interface{}); ok {
				event := map[string]interface{}{"type": "tokens-used"}
				if v, ok := u["inputTokens"]; ok {
					event["inputTokens"] = v
				}
				if v, ok := u["outputTokens"]; ok {
					event["outputTokens"] = v
				}
				onMessage(upstream.SSEMessage{Type: "model", Event: event})
			}
		}
		// Handle tool calls
		toolCalls := extractToolCallsFromResponse(msg)
		if len(toolCalls) > 0 {
			for _, call := range toolCalls {
				onMessage(upstream.SSEMessage{
					Type: "model.tool-call",
					Event: map[string]interface{}{
						"toolCallId": call.id,
						"toolName":   call.name,
						"input":      call.input,
					},
				})
				state.sawToolCall = true
			}
			if !state.finishSent {
				onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"finishReason": "tool-calls", "type": "finish"}})
				state.finishSent = true
			}
			return true // Break loop
		}
	}

	if state.textStarted {
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-end", "id": "0"}})
	}
	if state.reasoningStarted {
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "reasoning-end", "id": "0"}})
		state.reasoningStarted = false
	}
	if !state.finishSent {
		finishReason := "stop"
		if state.sawToolCall {
			finishReason = "tool-calls"
		}
		onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": finishReason}})
		state.finishSent = true
	}
	return true // Break loop
}

func (c *Client) dispatchFSOperation(
	msg map[string]interface{},
	onMessage func(upstream.SSEMessage),
	conn *websocket.Conn,
	wg *sync.WaitGroup,
	workdir string,
) {
	onMessage(upstream.SSEMessage{Type: "fs_operation", Event: msg})
	wg.Add(1)
	go func(m map[string]interface{}) {
		defer wg.Done()
		if err := c.handleFSOperation(conn, m, func(success bool, data interface{}, errMsg string) {
			if onMessage != nil {
				onMessage(upstream.SSEMessage{
					Type: "fs_operation_result",
					Event: map[string]interface{}{
						"success": success,
						"data":    data,
						"error":   errMsg,
						"op":      m,
					},
				})
			}
		}, workdir); err != nil {
			// Error handled inside respond or logged via debug
		}
	}(msg)
}

func (c *Client) handleModelEvent(
	msg map[string]interface{},
	state *requestState,
	onMessage func(upstream.SSEMessage),
) bool {
	event, ok := msg["event"].(map[string]interface{})
	if !ok {
		return false
	}
	eventType, _ := event["type"].(string)
	if state.suppressStarts && eventType == "stream-start" {
		return false
	}

	if state.preferCodingAgent {
		// Suppress only duplicate text/thinking segments to avoid redundancy
		// while allowing tool-calls, finish, and tokens usage to pass through.
		if eventType == "text-start" || eventType == "text-delta" || eventType == "text-end" ||
			eventType == "reasoning-start" || eventType == "reasoning-delta" || eventType == "reasoning-end" {
			return false
		}
	}

	if eventType == "tool-call" {
		state.sawToolCall = true
	}
	onMessage(upstream.SSEMessage{Type: "model", Event: event, Raw: msg})
	if eventType == "finish" {
		state.finishSent = true
		if reason, ok := event["finishReason"].(string); ok {
			if state.textStarted {
				onMessage(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "text-end", "id": "0"}})
			}
			if reason == "tool-calls" {
				return true
			}
			return true
		}
	}
	return false
}

func (c *Client) buildWSURLAIClient(token string) string {
	if c.config == nil {
		return ""
	}
	wsURL := strings.TrimSpace(c.config.OrchidsWSURL)
	if wsURL == "" {
		wsURL = "wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent"
	}
	sep := "?"
	if strings.Contains(wsURL, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%stoken=%s", wsURL, sep, urlEncode(token))
}

func (c *Client) buildWSRequestAIClient(req upstream.UpstreamRequest) (*orchidsWSRequest, error) {
	if c.config == nil {
		return nil, errors.New("server config unavailable")
	}
	systemText := extractSystemPrompt(req.Messages)
	if systemText == "" && len(req.System) > 0 {
		var sb strings.Builder
		for _, item := range req.System {
			sb.WriteString(item.Text)
			sb.WriteString("\n")
		}
		systemText = sb.String()
	}
	userText, currentToolResults := extractUserMessageAIClient(req.Messages)
	currentUserIdx := findCurrentUserMessageIndex(req.Messages)
	var historyMessages []prompt.Message
	if currentUserIdx >= 0 {
		historyMessages = req.Messages[:currentUserIdx]
	} else {
		historyMessages = req.Messages
	}
	chatHistory, historyToolResults := convertChatHistoryAIClient(historyMessages)
	toolResults := mergeToolResults(historyToolResults, currentToolResults)
	orchidsTools := convertOrchidsTools(req.Tools)
	attachmentUrls := extractAttachmentURLsAIClient(req.Messages)

	promptText := ""
	if req.Prompt != "" {
		promptText = req.Prompt
		// 非 AIClient 模式下，若 prompt 已包含完整历史，则避免 chatHistory 重复注入。
		if c.config == nil || !strings.EqualFold(strings.TrimSpace(c.config.OrchidsImpl), "aiclient") {
			chatHistory = nil
		}
	} else {
		promptText = buildLocalAssistantPrompt(systemText, userText, req.Model)
		if !req.NoThinking && !isSuggestionModeText(userText) {
			promptText = injectThinkingPrefix(promptText)
		}
	}

	if req.NoTools {
		orchidsTools = nil
		toolResults = nil
	}

	agentMode := normalizeAIClientModel(req.Model)

	chatSessionID := req.ChatSessionID
	if chatSessionID == "" {
		chatSessionID = "chat_" + randomSuffix(12)
	}

	payload := map[string]interface{}{
		"projectId":      nil,
		"chatSessionId":  chatSessionID,
		"prompt":         promptText,
		"agentMode":      agentMode,
		"mode":           "agent",
		"chatHistory":    chatHistory,
		"attachmentUrls": attachmentUrls,
		"currentPage":    nil,
		"email":          c.config.Email,
		"isLocal":        false, // MUST be false to avoid upstream indexing loop
		"isFixingErrors": false,
		"fileStructure":  nil,
		"userId":         c.config.UserID,
		"apiVersion":     2,
	}
	// Do NOT send localWorkingDirectory to avoid triggering the crawler
	if len(orchidsTools) > 0 {
		payload["tools"] = orchidsTools
	}
	if len(toolResults) > 0 {
		payload["toolResults"] = toolResults
	}

	return &orchidsWSRequest{
		Type: "user_request",
		Data: payload,
	}, nil
}

func isSuggestionModeText(text string) bool {
	normalized := strings.ToLower(text)
	return strings.Contains(normalized, "suggestion mode")
}

func defaultUserID(id string) string {
	if strings.TrimSpace(id) == "" {
		return "local_user"
	}
	return id
}

func normalizeAIClientModel(model string) string {
	mapped := strings.TrimSpace(model)
	if mapped == "" {
		mapped = orchidsAIClientDefaultModel
	}
	switch mapped {
	case "claude-haiku-4-5":
		mapped = "claude-sonnet-4-5"
	case "claude-opus-4-5":
		mapped = "claude-opus-4.5"
	}
	if !containsString(orchidsAIClientModels, mapped) {
		return orchidsAIClientDefaultModel
	}
	return mapped
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func extractUserMessageAIClient(messages []prompt.Message) (string, []orchidsToolResult) {
	var toolResults []orchidsToolResult
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		text, results := extractMessageTextAIClient(msg.Content)
		if len(results) > 0 {
			toolResults = append(toolResults, results...)
		}
		if strings.TrimSpace(text) != "" || len(results) > 0 {
			return text, toolResults
		}
	}
	return "", toolResults
}

func extractMessageTextAIClient(content prompt.MessageContent) (string, []orchidsToolResult) {
	if content.IsString() {
		text := stripSystemReminders(content.GetText())
		if text == "" {
			return "", nil
		}
		return text, nil
	}
	var parts []string
	var toolResults []orchidsToolResult
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			text := stripSystemReminders(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			text := formatToolResultContentLocal(block.Content)
			text = strings.ReplaceAll(text, "<tool_use_error>", "")
			text = strings.ReplaceAll(text, "</tool_use_error>", "")
			text = stripSystemReminders(text)
			if text != "" {
				parts = append(parts, text)
			}
			status := "success"
			if block.IsError {
				status = "error"
			}
			toolResults = append(toolResults, orchidsToolResult{
				Content:   []map[string]string{{"text": text}},
				Status:    status,
				ToolUseID: block.ToolUseID,
			})
		case "image":
			parts = append(parts, formatMediaHint(block))
		case "document":
			parts = append(parts, formatMediaHint(block))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), toolResults
}

func convertChatHistoryAIClient(messages []prompt.Message) ([]map[string]string, []orchidsToolResult) {
	var history []map[string]string
	var toolResults []orchidsToolResult
	for _, msg := range messages {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if msg.Role == "user" {
			if msg.Content.IsString() {
				text := stripSystemReminders(msg.Content.GetText())
				if text != "" {
					history = append(history, map[string]string{
						"role":    "user",
						"content": text,
					})
				}
				continue
			}
			blocks := msg.Content.GetBlocks()
			var textParts []string
			hasValidContent := false
			for _, block := range blocks {
				switch block.Type {
				case "text":
					text := stripSystemReminders(block.Text)
					if text != "" {
						textParts = append(textParts, text)
						hasValidContent = true
					}
				case "tool_result":
					contentText := formatToolResultContentLocal(block.Content)
					contentText = strings.ReplaceAll(contentText, "<tool_use_error>", "")
					contentText = strings.ReplaceAll(contentText, "</tool_use_error>", "")
					contentText = stripSystemReminders(contentText)
					if contentText != "" {
						textParts = append(textParts, contentText)
						hasValidContent = true
					}
					status := "success"
					if block.IsError {
						status = "error"
					}
					toolResults = append(toolResults, orchidsToolResult{
						Content:   []map[string]string{{"text": contentText}},
						Status:    status,
						ToolUseID: block.ToolUseID,
					})
				case "image":
					textParts = append(textParts, formatMediaHint(block))
					hasValidContent = true
				case "document":
					textParts = append(textParts, formatMediaHint(block))
					hasValidContent = true
				}
			}
			if !hasValidContent {
				continue
			}
			text := strings.TrimSpace(strings.Join(textParts, "\n"))
			if text != "" {
				history = append(history, map[string]string{
					"role":    "user",
					"content": text,
				})
			}
			continue
		}

		if msg.Content.IsString() {
			text := stripSystemReminders(msg.Content.GetText())
			if text == "" {
				continue
			}
			history = append(history, map[string]string{
				"role":    "assistant",
				"content": text,
			})
			continue
		}
		var parts []string
		for _, block := range msg.Content.GetBlocks() {
			switch block.Type {
			case "text":
				text := stripSystemReminders(block.Text)
				if text != "" {
					parts = append(parts, text)
				}
			case "tool_use":
				inputJSON, _ := json.Marshal(block.Input)
				parts = append(parts, fmt.Sprintf("[Used tool: %s with input: %s]", block.Name, string(inputJSON)))
			case "image":
				parts = append(parts, formatMediaHint(block))
			case "document":
				parts = append(parts, formatMediaHint(block))
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			continue
		}
		history = append(history, map[string]string{
			"role":    "assistant",
			"content": text,
		})
	}
	return history, toolResults
}

func extractAttachmentURLsAIClient(messages []prompt.Message) []string {
	seen := map[string]bool{}
	var urls []string
	for _, msg := range messages {
		if msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "image" && block.Type != "document" {
				continue
			}
			url := ""
			if block.Source != nil {
				url = strings.TrimSpace(block.Source.URL)
			}
			if url == "" {
				url = strings.TrimSpace(block.URL)
			}
			if url == "" || seen[url] {
				continue
			}
			seen[url] = true
			urls = append(urls, url)
		}
	}
	return urls
}

func formatMediaHint(block prompt.ContentBlock) string {
	sourceType := "unknown"
	mediaType := "unknown"
	sizeHint := ""
	if block.Source != nil {
		if strings.TrimSpace(block.Source.Type) != "" {
			sourceType = block.Source.Type
		}
		if strings.TrimSpace(block.Source.MediaType) != "" {
			mediaType = block.Source.MediaType
		}
		if block.Source.Data != "" {
			approx := int(float64(len(block.Source.Data)) * 0.75)
			sizeHint = fmt.Sprintf(" bytes≈%d", approx)
		}
	}
	switch block.Type {
	case "image":
		return fmt.Sprintf("[Image %s %s%s]", mediaType, sourceType, sizeHint)
	case "document":
		return fmt.Sprintf("[Document %s%s]", sourceType, sizeHint)
	default:
		return "[Document unknown]"
	}
}
