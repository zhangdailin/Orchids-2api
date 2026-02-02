package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/perf"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/util"
)

type streamHandler struct {
	// Configuration
	config           *config.Config
	isStream         bool
	toolCallMode     string
	suppressThinking bool
	useUpstreamUsage bool
	outputTokenMode  string
	responseFormat   string // "anthropic" or "openai"

	// HTTP Response
	w       http.ResponseWriter
	flusher http.Flusher

	// State
	mu                       sync.Mutex
	outputMu                 sync.Mutex
	blockIndex               int
	msgID                    string
	startTime                time.Time
	hasReturn                bool
	finalStopReason          string
	outputTokens             int
	inputTokens              int
	activeThinkingBlockIndex int
	activeTextBlockIndex     int
	activeBlockType          string // "thinking", "text", "tool_use"

	// Buffers and Builders
	responseText          *strings.Builder
	outputBuilder         *strings.Builder
	textBlockBuilders     map[int]*strings.Builder
	thinkingBlockBuilders map[int]*strings.Builder
	contentBlocks         []map[string]interface{}
	currentTextIndex      int

	// Tool Handling
	toolBlocks         map[string]int
	pendingToolCalls   []toolCall
	toolInputNames     map[string]string
	toolInputBuffers   map[string]*strings.Builder
	toolInputHadDelta  map[string]bool
	toolCallHandled    map[string]bool
	currentToolInputID string
	toolCallCount      int
	autoPendingCalls   []toolCall

	// Internal Tools & Resolution
	allowedTools          map[string]string
	allowedIndex          []toolNameInfo
	hasToolList           bool
	internalToolResults   []safeToolResult
	internalNeedsFollowup bool
	preflightResults      []safeToolResult
	shouldLocalFallback   bool

	// Edit Tool State
	editFilePath  string
	editNewString string

	// Throttling
	lastScanTime time.Time

	// Logger
	logger *debug.Logger
}

func newStreamHandler(
	cfg *config.Config,
	w http.ResponseWriter,
	// r *http.Request, // Not used directly in storage
	logger *debug.Logger,
	toolCallMode string,
	suppressThinking bool,
	allowedTools map[string]string,
	allowedIndex []toolNameInfo,
	preflightResults []safeToolResult,
	shouldLocalFallback bool,
	isStream bool,
	responseFormat string,
) *streamHandler {
	var flusher http.Flusher
	if isStream {
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
	}

	outputTokenMode := strings.ToLower(strings.TrimSpace(cfg.OutputTokenMode))
	if outputTokenMode == "" {
		outputTokenMode = "final"
	}

	h := &streamHandler{
		config:              cfg,
		w:                   w,
		flusher:             flusher,
		isStream:            isStream,
		logger:              logger,
		toolCallMode:        toolCallMode,
		suppressThinking:    suppressThinking,
		allowedTools:        allowedTools,
		allowedIndex:        allowedIndex,
		hasToolList:         len(allowedTools) > 0,
		preflightResults:    preflightResults,
		shouldLocalFallback: shouldLocalFallback,
		outputTokenMode:     outputTokenMode,
		responseFormat:      responseFormat,

		blockIndex:               -1,
		toolBlocks:               make(map[string]int),
		responseText:             perf.AcquireStringBuilder(),
		outputBuilder:            perf.AcquireStringBuilder(),
		textBlockBuilders:        make(map[int]*strings.Builder),
		thinkingBlockBuilders:    make(map[int]*strings.Builder),
		toolInputNames:           make(map[string]string),
		toolInputBuffers:         make(map[string]*strings.Builder),
		toolInputHadDelta:        make(map[string]bool),
		toolCallHandled:          make(map[string]bool),
		msgID:                    fmt.Sprintf("msg_%d", time.Now().UnixMilli()),
		startTime:                time.Now(),
		currentTextIndex:         -1,
		activeThinkingBlockIndex: -1,
		activeTextBlockIndex:     -1,
		activeBlockType:          "",
	}
	return h
}

func (h *streamHandler) release() {
	perf.ReleaseStringBuilder(h.responseText)
	perf.ReleaseStringBuilder(h.outputBuilder)
	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	for _, sb := range h.thinkingBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	for _, sb := range h.toolInputBuffers {
		perf.ReleaseStringBuilder(sb)
	}
}

func (h *streamHandler) writeSSE(event, data string) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasReturn {
		return
	}
	if h.responseFormat == "openai" {
		h.writeOpenAISSE(event, data)
		return
	}

	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}

	h.logger.LogOutputSSE(event, data)
}

func (h *streamHandler) writeOpenAISSE(event, data string) {
	// Transform Anthropic event to OpenAI chunk
	chunk := map[string]interface{}{
		"id":      h.msgID,
		"object":  "chat.completion.chunk",
		"created": h.startTime.Unix(),
		"model":   "", // Should be set if available, but optional in chunk often
		"choices": []map[string]interface{}{},
	}

	var parsedData map[string]interface{}
	if err := json.Unmarshal([]byte(data), &parsedData); err != nil {
		return
	}

	choice := map[string]interface{}{
		"index": 0,
		"delta": map[string]interface{}{},
	}

	switch event {
	case "message_start":
		if msg, ok := parsedData["message"].(map[string]interface{}); ok {
			choice["delta"] = map[string]interface{}{"role": "assistant"}
			if model, ok := msg["model"].(string); ok {
				chunk["model"] = model
			}
		}
	case "content_block_start":
		if cb, ok := parsedData["content_block"].(map[string]interface{}); ok {
			if cb["type"] == "text" {
				if text, ok := cb["text"].(string); ok && text != "" {
					choice["delta"] = map[string]interface{}{"content": text}
				}
			} else if cb["type"] == "tool_use" {
				choice["delta"] = map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{
							"index": 0,
							"id":    cb["id"],
							"type":  "function",
							"function": map[string]interface{}{
								"name":      cb["name"],
								"arguments": "",
							},
						},
					},
				}
			}
		}
	case "content_block_delta":
		if delta, ok := parsedData["delta"].(map[string]interface{}); ok {
			if delta["type"] == "text_delta" {
				choice["delta"] = map[string]interface{}{"content": delta["text"]}
			} else if delta["type"] == "input_json_delta" {
				choice["delta"] = map[string]interface{}{
					"tool_calls": []map[string]interface{}{
						{
							"index": 0,
							"function": map[string]interface{}{
								"arguments": delta["partial_json"],
							},
						},
					},
				}
			} else if delta["type"] == "thinking_delta" {
				// Map thinking to reasoning_content for supported clients (e.g. DeepSeek/similar)
				choice["delta"] = map[string]interface{}{"reasoning_content": delta["thinking"]}
			}
		}
	case "message_delta":
		if delta, ok := parsedData["delta"].(map[string]interface{}); ok {
			if stopReason, ok := delta["stop_reason"].(string); ok {
				choice["finish_reason"] = stopReason
			}
		}
		// OpenAI expects finish_reason in the last chunk usually, or separate
		choice["delta"] = map[string]interface{}{} // Empty delta for usage/stop
	case "message_stop":
		choice["finish_reason"] = "stop" // Fallback
		choice["delta"] = map[string]interface{}{}
	case "content_block_stop":
		return // Ignore internal block stops for OpenAI
	default:
		return // Ignore other events
	}

	// Don't send empty updates unless it's a role or finish_reason
	delta, _ := choice["delta"].(map[string]interface{})
	if len(delta) == 0 && choice["finish_reason"] == nil {
		return
	}

	chunk["choices"] = []interface{}{choice}
	
	bytes, _ := json.Marshal(chunk)
	fmt.Fprintf(h.w, "data: %s\n\n", string(bytes))
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

func (h *streamHandler) writeFinalSSE(event, data string) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.responseFormat == "openai" {
		h.writeOpenAISSE(event, data)
		// Send [DONE] at the very end
		if event == "message_stop" {
			fmt.Fprintf(h.w, "data: [DONE]\n\n")
			if h.flusher != nil {
				h.flusher.Flush()
			}
		}
		return
	}

	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}

	h.logger.LogOutputSSE(event, data)
}

func (h *streamHandler) writeKeepAlive() {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.w, ": keep-alive\n\n")
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

func (h *streamHandler) addOutputTokens(text string) {
	if text == "" {
		return
	}
	h.outputMu.Lock()
	if !h.useUpstreamUsage {
		h.outputBuilder.WriteString(text)
	}
	h.outputMu.Unlock()
}

func (h *streamHandler) finalizeOutputTokens() {
	h.outputMu.Lock()
	defer h.outputMu.Unlock()

	if h.useUpstreamUsage {
		return
	}

	text := h.outputBuilder.String()
	h.outputTokens = tiktoken.EstimateTextTokens(text)
}

func (h *streamHandler) setUsageTokens(input, output int) {
	h.outputMu.Lock()
	if input >= 0 {
		h.inputTokens = input
	}
	if output >= 0 {
		h.outputTokens = output
		h.useUpstreamUsage = true
	}
	h.outputMu.Unlock()
}

func (h *streamHandler) resetRoundState() {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Ensure any currently open block is closed before resetting state
	h.closeActiveBlockLocked()

	// Do NOT reset h.blockIndex = -1 here.
	// Preserving blockIndex across retries ensures that a retry starts with NEW indices.
	// Index collisions (re-sending index 0) cause "Mismatched content block type" on the client.

	h.activeThinkingBlockIndex = -1
	h.activeTextBlockIndex = -1
	h.activeBlockType = ""
	h.hasReturn = false

	clear(h.toolBlocks)
	h.responseText.Reset()
	h.contentBlocks = nil
	h.currentTextIndex = -1

	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	clear(h.textBlockBuilders)

	for _, sb := range h.thinkingBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	clear(h.thinkingBlockBuilders)

	h.pendingToolCalls = nil
	clear(h.toolInputNames)

	for _, sb := range h.toolInputBuffers {
		perf.ReleaseStringBuilder(sb)
	}
	clear(h.toolInputBuffers)

	clear(h.toolInputHadDelta)
	clear(h.toolCallHandled)
	h.currentToolInputID = ""
	h.toolCallCount = 0
	h.outputTokens = 0
	h.outputBuilder.Reset()
	h.useUpstreamUsage = false
	h.finalStopReason = ""
}

func (h *streamHandler) shouldEmitToolCalls(stopReason string) bool {
	switch h.toolCallMode {
	case "auto":
		return stopReason == "tool_use"
	case "internal":
		return false
	default:
		return true
	}
}

func (h *streamHandler) emitToolCallNonStream(call toolCall) {
	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	fixedInput := fixToolInput(call.input)
	var inputValue interface{}
	if err := json.Unmarshal([]byte(fixedInput), &inputValue); err != nil {
		inputValue = map[string]interface{}{}
	}
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type":  "tool_use",
		"id":    call.id,
		"name":  call.name,
		"input": inputValue,
	})
}

func (h *streamHandler) emitToolCallStream(call toolCall, idx int, write func(event, data string)) {
	if call.id == "" {
		return
	}
	if idx < 0 {
		h.mu.Lock()
		h.blockIndex++
		idx = h.blockIndex
		h.mu.Unlock()
	}

	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	fixedInput := fixToolInput(call.input)

	startMap := perf.AcquireMap()
	startMap["type"] = "content_block_start"
	startMap["index"] = idx

	contentBlock := perf.AcquireMap()
	contentBlock["type"] = "tool_use"
	contentBlock["id"] = call.id
	contentBlock["name"] = call.name
	contentBlock["input"] = perf.AcquireMap() // Empty map
	startMap["content_block"] = contentBlock

	startData, _ := json.Marshal(startMap)
	perf.ReleaseMap(contentBlock["input"].(map[string]interface{}))
	perf.ReleaseMap(contentBlock)
	perf.ReleaseMap(startMap)
	write("content_block_start", string(startData))

	deltaMap := perf.AcquireMap()
	deltaMap["type"] = "content_block_delta"
	deltaMap["index"] = idx

	deltaContent := perf.AcquireMap()
	deltaContent["type"] = "input_json_delta"
	deltaContent["partial_json"] = fixedInput
	deltaMap["delta"] = deltaContent

	deltaData, _ := json.Marshal(deltaMap)
	perf.ReleaseMap(deltaContent)
	perf.ReleaseMap(deltaMap)
	write("content_block_delta", string(deltaData))

	stopMap := perf.AcquireMap()
	stopMap["type"] = "content_block_stop"
	stopMap["index"] = idx
	stopData, _ := json.Marshal(stopMap)
	perf.ReleaseMap(stopMap)
	write("content_block_stop", string(stopData))
}

func (h *streamHandler) flushPendingToolCalls(stopReason string, write func(event, data string)) {
	if !h.shouldEmitToolCalls(stopReason) {
		return
	}

	h.mu.Lock()
	calls := make([]toolCall, len(h.pendingToolCalls))
	copy(calls, h.pendingToolCalls)
	h.pendingToolCalls = nil
	h.mu.Unlock()

	for _, call := range calls {
		if h.isStream {
			h.emitToolCallStream(call, -1, write)
		} else {
			h.emitToolCallNonStream(call)
		}
	}
}

func (h *streamHandler) overrideWithLocalContext() {
	if h.isStream || !h.shouldLocalFallback {
		return
	}
	pwd := extractPreflightPwd(h.preflightResults)
	currentText := strings.TrimSpace(h.responseText.String())
	if currentText != "" && pwd != "" && strings.Contains(currentText, pwd) {
		return
	}
	fallback := buildLocalFallbackResponse(h.preflightResults)
	if fallback == "" {
		return
	}
	h.responseText.Reset()
	h.responseText.WriteString(fallback)
	h.contentBlocks = []map[string]interface{}{
		{
			"type": "text",
			"text": fallback,
		},
	}
	h.textBlockBuilders = map[int]*strings.Builder{}
	h.currentTextIndex = -1
	h.outputBuilder.Reset()
	h.outputBuilder.WriteString(fallback)
	h.outputTokens = 0
}

func (h *streamHandler) finishResponse(stopReason string) {
	if stopReason == "tool_use" {
		h.mu.Lock()
		hasToolCalls := h.toolCallCount > 0 || len(h.pendingToolCalls) > 0
		h.mu.Unlock()
		if !hasToolCalls {
			stopReason = "end_turn"
		}
	}
	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	h.hasReturn = true
	h.finalStopReason = stopReason
	h.mu.Unlock()

	if h.isStream {
		h.flushPendingToolCalls(stopReason, h.writeFinalSSE)
		h.finalizeOutputTokens()
		deltaMap := perf.AcquireMap()
		deltaMap["type"] = "message_delta"
		deltaDelta := perf.AcquireMap()
		deltaDelta["stop_reason"] = stopReason
		deltaUsage := perf.AcquireMap()
		deltaUsage["output_tokens"] = h.outputTokens
		deltaMap["delta"] = deltaDelta
		deltaMap["usage"] = deltaUsage
		deltaData, _ := json.Marshal(deltaMap)
		perf.ReleaseMap(deltaUsage)
		perf.ReleaseMap(deltaDelta)
		perf.ReleaseMap(deltaMap)
		h.writeFinalSSE("message_delta", string(deltaData))

		stopMap := perf.AcquireMap()
		stopMap["type"] = "message_stop"
		stopData, _ := json.Marshal(stopMap)
		perf.ReleaseMap(stopMap)
		h.writeFinalSSE("message_stop", string(stopData))
	} else {
		h.flushPendingToolCalls(stopReason, h.writeFinalSSE)
		h.overrideWithLocalContext()
		h.finalizeOutputTokens()
	}

	// 记录摘要
	h.logger.LogSummary(h.inputTokens, h.outputTokens, time.Since(h.startTime), stopReason)
	slog.Debug("Request completed", "input_tokens", h.inputTokens, "output_tokens", h.outputTokens, "duration", time.Since(h.startTime))
}

func (h *streamHandler) resolveToolName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if !h.hasToolList {
		return name, true
	}
	if resolved, ok := h.allowedTools[strings.ToLower(name)]; ok {
		return resolved, true
	}
	return "", false
}

func (h *streamHandler) ensureBlock(blockType string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	// If already in a block of a different type, close it
	if h.activeBlockType != "" && h.activeBlockType != blockType {
		h.closeActiveBlockLocked()
	}

	// If already in the correct block type, return current index
	if h.activeBlockType == blockType {
		if blockType == "thinking" {
			return h.activeThinkingBlockIndex
		}
		if blockType == "text" {
			return h.activeTextBlockIndex
		}
	}

	// Start new block
	h.blockIndex++
	idx := h.blockIndex
	h.activeBlockType = blockType

	var startData []byte
	switch blockType {
	case "thinking":
		h.activeThinkingBlockIndex = idx
		if !h.isStream {
			h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
				"type": "thinking",
			})
			h.activeThinkingBlockIndex = len(h.contentBlocks) - 1
			h.thinkingBlockBuilders[h.activeThinkingBlockIndex] = perf.AcquireStringBuilder()
			idx = h.activeThinkingBlockIndex
		}

		m := perf.AcquireMap()
		m["type"] = "content_block_start"
		m["index"] = idx

		cb := perf.AcquireMap()
		cb["type"] = "thinking"
		cb["thinking"] = ""
		m["content_block"] = cb

		startData, _ = json.Marshal(m)
		perf.ReleaseMap(cb)
		perf.ReleaseMap(m)
	case "text":
		h.activeTextBlockIndex = idx
		if !h.isStream {
			h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
				"type": "text",
			})
			h.currentTextIndex = len(h.contentBlocks) - 1
			h.textBlockBuilders[h.currentTextIndex] = perf.AcquireStringBuilder()
		}

		m := perf.AcquireMap()
		m["type"] = "content_block_start"
		m["index"] = idx

		cb := perf.AcquireMap()
		cb["type"] = "text"
		cb["text"] = ""
		m["content_block"] = cb

		startData, _ = json.Marshal(m)
		perf.ReleaseMap(cb)
		perf.ReleaseMap(m)
	}

	if len(startData) > 0 {
		h.writeSSELocked("content_block_start", string(startData))
	}

	return idx
}

func (h *streamHandler) closeActiveBlock() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeActiveBlockLocked()
}

func (h *streamHandler) closeActiveBlockLocked() {
	if h.activeBlockType == "" {
		return
	}

	var idx int
	switch h.activeBlockType {
	case "thinking":
		idx = h.activeThinkingBlockIndex
		h.activeThinkingBlockIndex = -1
	case "text":
		idx = h.activeTextBlockIndex
		h.activeTextBlockIndex = -1
	default:
		// tool_use and others are usually handled as single-event blocks or managed separately
		h.activeBlockType = ""
		return
	}

	h.activeBlockType = ""

	m := perf.AcquireMap()
	m["type"] = "content_block_stop"
	m["index"] = idx
	stopData, _ := json.Marshal(m)
	perf.ReleaseMap(m)

	h.writeSSELocked("content_block_stop", string(stopData))
}

func (h *streamHandler) writeSSELocked(event, data string) {
	if !h.isStream {
		return
	}
	if h.responseFormat == "openai" {
		h.writeOpenAISSE(event, data)
		return
	}
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}
	h.logger.LogOutputSSE(event, data)
	// Log to slog for server.log visibility
	slog.Debug("SSE Out", "event", event, "data_len", len(data))
}

// Event Handlers

func (h *streamHandler) processAutoPendingCalls() {
	if len(h.autoPendingCalls) == 0 {
		return
	}

	h.mu.Lock()
	calls := make([]toolCall, len(h.autoPendingCalls))
	copy(calls, h.autoPendingCalls)
	h.autoPendingCalls = nil
	h.mu.Unlock()

	if len(calls) == 0 {
		return
	}

	results := make([]safeToolResult, len(calls))
	util.ParallelFor(len(calls), func(i int) {
		results[i] = executeToolCall(calls[i], h.config)
	})

	// Append results in order
	for _, res := range results {
		h.internalToolResults = append(h.internalToolResults, res)
	}
}

func (h *streamHandler) handleToolCall(call toolCall) {
	if call.id == "" {
		return
	}
	if h.toolCallMode == "internal" {
		result := executeSafeTool(call)
		h.internalToolResults = append(h.internalToolResults, result)
		return
	}
	if h.toolCallMode == "auto" {
		if !h.isStream {
			h.emitToolCallNonStream(call)
		} else {
			// Emit text notification instead of tool_use to avoid client side execution
			notification := fmt.Sprintf("\n[Running tool: %s]\n", call.name)

			h.mu.Lock()
			h.blockIndex++
			idx := h.blockIndex
			h.mu.Unlock()

			startMap := perf.AcquireMap()
			startMap["type"] = "content_block_start"
			startMap["index"] = idx
			startContent := perf.AcquireMap()
			startContent["type"] = "text"
			startContent["text"] = ""
			startMap["content_block"] = startContent
			startData, _ := json.Marshal(startMap)
			perf.ReleaseMap(startContent)
			perf.ReleaseMap(startMap)
			h.writeSSE("content_block_start", string(startData))

			deltaMap := perf.AcquireMap()
			deltaMap["type"] = "content_block_delta"
			deltaMap["index"] = idx
			deltaContent := perf.AcquireMap()
			deltaContent["type"] = "text_delta"
			deltaContent["text"] = notification
			deltaMap["delta"] = deltaContent
			deltaData, _ := json.Marshal(deltaMap)
			perf.ReleaseMap(deltaContent)
			perf.ReleaseMap(deltaMap)
			h.writeSSE("content_block_delta", string(deltaData))

			stopMap := perf.AcquireMap()
			stopMap["type"] = "content_block_stop"
			stopMap["index"] = idx
			stopData, _ := json.Marshal(stopMap)
			perf.ReleaseMap(stopMap)
			h.writeSSE("content_block_stop", string(stopData))
		}

		h.mu.Lock()
		h.autoPendingCalls = append(h.autoPendingCalls, call)
		h.mu.Unlock()
		return
	}
	h.mu.Lock()
	h.toolCallCount++
	h.mu.Unlock()
	// Default pending behavior (proxy mode removed)
	h.mu.Lock()
	h.pendingToolCalls = append(h.pendingToolCalls, call)
	h.mu.Unlock()
}

func (h *streamHandler) handleMessage(msg orchids.SSEMessage) {
	if h.config.DebugEnabled && msg.Type != "content_block_delta" {
		slog.Debug("Incoming SSE", "type", msg.Type)
	}
	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()

	eventKey := msg.Type
	if msg.Type == "model" && msg.Event != nil {
		if evtType, ok := msg.Event["type"].(string); ok {
			eventKey = "model." + evtType
		}
	}
	if h.suppressThinking && strings.HasPrefix(eventKey, "model.reasoning-") {
		return
	}

	getUsageInt := func(usage map[string]interface{}, key string) (int, bool) {
		if usage == nil {
			return 0, false
		}
		if raw, ok := usage[key]; ok {
			switch v := raw.(type) {
			case float64:
				return int(v), true
			case int:
				return v, true
			case json.Number:
				if n, err := v.Int64(); err == nil {
					return int(n), true
				}
			}
		}
		return 0, false
	}

	switch eventKey {
	case "model.reasoning-start":
		h.ensureBlock("thinking")

	case "model.reasoning-delta":
		h.mu.Lock()
		idx := h.activeThinkingBlockIndex
		h.mu.Unlock()
		if idx < 0 {
			// If we get delta but no thinking block is active, try to ensure one
			idx = h.ensureBlock("thinking")
		}
		delta, _ := msg.Event["delta"].(string)
		if h.isStream {
			h.addOutputTokens(delta)
		} else {
			if h.activeThinkingBlockIndex >= 0 && h.activeThinkingBlockIndex < len(h.contentBlocks) {
				builder, ok := h.thinkingBlockBuilders[h.activeThinkingBlockIndex]
				if !ok {
					builder = perf.AcquireStringBuilder()
					h.thinkingBlockBuilders[h.activeThinkingBlockIndex] = builder
				}
				builder.WriteString(delta)
			}
		}
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = idx
		deltaMap := perf.AcquireMap()
		deltaMap["type"] = "thinking_delta"
		deltaMap["thinking"] = delta
		m["delta"] = deltaMap
		data, _ := json.Marshal(m)
		h.writeSSE("content_block_delta", string(data))
		perf.ReleaseMap(deltaMap)
		perf.ReleaseMap(m)

	case "model.reasoning-end":
		h.closeActiveBlock()

	case "model.text-start":
		h.ensureBlock("text")

	case "model.text-delta":
		h.mu.Lock()
		idx := h.activeTextBlockIndex
		h.mu.Unlock()
		if idx < 0 {
			// If we get delta but no text block is active, try to ensure one
			idx = h.ensureBlock("text")
		}
		delta, _ := msg.Event["delta"].(string)
		h.addOutputTokens(delta)
		if !h.isStream {
			h.responseText.WriteString(delta)
			if h.currentTextIndex >= 0 && h.currentTextIndex < len(h.contentBlocks) {
				builder, ok := h.textBlockBuilders[h.currentTextIndex]
				if !ok {
					builder = perf.AcquireStringBuilder()
					h.textBlockBuilders[h.currentTextIndex] = builder
				}
				builder.WriteString(delta)
			}
		}
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = idx
		deltaMap := perf.AcquireMap()
		deltaMap["type"] = "text_delta"
		deltaMap["text"] = delta
		m["delta"] = deltaMap
		data, _ := json.Marshal(m)
		h.writeSSE("content_block_delta", string(data))
		perf.ReleaseMap(deltaMap)
		perf.ReleaseMap(m)

	case "model.text-end":
		h.closeActiveBlock()

	case "coding_agent.initializing":
		if data, ok := msg.Event["data"].(map[string]interface{}); ok {
			if message, ok := data["message"].(string); ok {
				idx := h.ensureBlock("text")
				deltaMap := perf.AcquireMap()
				deltaMap["type"] = "content_block_delta"
				deltaMap["index"] = idx
				deltaContent := perf.AcquireMap()
				deltaContent["type"] = "text_delta"
				deltaContent["text"] = fmt.Sprintf("\n[System: %s]\n", message)
				deltaMap["delta"] = deltaContent
				deltaData, _ := json.Marshal(deltaMap)
				perf.ReleaseMap(deltaContent)
				perf.ReleaseMap(deltaMap)
				h.writeSSE("content_block_delta", string(deltaData))
			}
		}

	case "fs_operation":
		// Throttle keep-alives to avoid flooding
		h.mu.Lock()
		if time.Since(h.lastScanTime) < 1*time.Second {
			h.mu.Unlock()
			return
		}
		h.lastScanTime = time.Now()
		h.mu.Unlock()

		if h.config.DebugEnabled {
			slog.Debug("Upstream active", "op", msg.Event["operation"])
		}
		h.writeKeepAlive()
		return

	case "fs_operation_result":
		success, _ := msg.Event["success"].(bool)
		data := msg.Event["data"]
		errMsg, _ := msg.Event["error"].(string)
		op, _ := msg.Event["op"].(map[string]interface{})

		_ = op["id"]
		opName, _ := op["operation"].(string)

		// Map orchids operation back to Claude tool name if possible
		// mappedName := client.DefaultToolMapper.FromOrchids(opName)
		_ = orchids.DefaultToolMapper.FromOrchids(opName)

		output := ""
		if !success {
			output = errMsg
			if output == "" {
				output = "Operation failed"
			}
		} else {
			if data != nil {
				if s, ok := data.(string); ok {
					output = s
				} else {
					jsonData, _ := json.Marshal(data)
					output = string(jsonData)
				}
			}
		}

		/*
			// FIX: Do not treat fs_operation as a tool call requiring follow-up.
			// The upstream agent handles this inline via WebSocket.
			h.mu.Lock()
			h.internalToolResults = append(h.internalToolResults, safeToolResult{
				call: toolCall{
					id:   opID,
					name: mappedName,
				},
				input:   op,
				output:  output,
				isError: !success,
			})
			// h.internalNeedsFollowup = true
			h.mu.Unlock()
		*/

	case "model.tool-input-start":
		h.closeActiveBlock() // Tool input starts a separate block mechanism
		toolID, _ := msg.Event["id"].(string)
		toolName, _ := msg.Event["toolName"].(string)
		if toolID == "" || toolName == "" {
			return
		}
		h.currentToolInputID = toolID
		h.toolInputNames[toolID] = toolName
		h.toolInputBuffers[toolID] = perf.AcquireStringBuilder()
		h.toolInputHadDelta[toolID] = false
		if !h.isStream || (h.toolCallMode != "proxy" && h.toolCallMode != "auto") {
			return
		}
		mappedName := mapOrchidsToolName(toolName, "", h.allowedIndex, h.allowedTools)
		finalName, ok := h.resolveToolName(mappedName)
		if !ok {
			return
		}
		h.addOutputTokens(finalName)
		h.mu.Lock()
		h.blockIndex++
		idx := h.blockIndex
		h.toolBlocks[toolID] = idx
		h.toolCallCount++
		h.mu.Unlock()
		startMap := perf.AcquireMap()
		startMap["type"] = "content_block_start"
		startMap["index"] = idx
		startContent := perf.AcquireMap()
		startContent["type"] = "tool_use"
		startContent["id"] = toolID
		startContent["name"] = finalName
		startInput := perf.AcquireMap()
		startContent["input"] = startInput
		startMap["content_block"] = startContent
		startData, _ := json.Marshal(startMap)
		perf.ReleaseMap(startInput)
		perf.ReleaseMap(startContent)
		perf.ReleaseMap(startMap)
		h.writeSSE("content_block_start", string(startData))

	case "model.tool-input-delta":
		toolID, _ := msg.Event["id"].(string)
		delta, _ := msg.Event["delta"].(string)
		if toolID == "" {
			return
		}
		if buf, ok := h.toolInputBuffers[toolID]; ok {
			buf.WriteString(delta)
		}
		if delta != "" {
			h.toolInputHadDelta[toolID] = true
		}
		if !h.isStream || (h.toolCallMode != "proxy" && h.toolCallMode != "auto") || delta == "" {
			return
		}
		idx, ok := h.toolBlocks[toolID]
		if !ok {
			return
		}
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = idx
		deltaMap := perf.AcquireMap()
		deltaMap["type"] = "input_json_delta"
		deltaMap["partial_json"] = delta
		m["delta"] = deltaMap
		deltaData, _ := json.Marshal(m)
		h.writeSSE("content_block_delta", string(deltaData))
		perf.ReleaseMap(deltaMap)
		perf.ReleaseMap(m)

	case "coding_agent.Edit.edit.started":
		// Redundant: handled via standard model.tool-call
		return

	case "coding_agent.Edit.edit.chunk":
		// Redundant: handled via standard model.tool-call
		return

	case "coding_agent.Edit.edit.completed":
		// Redundant: handled via standard model.tool-call
		h.editFilePath = ""
		h.editNewString = ""
		return

	case "model.tool-input-end":
		toolID, _ := msg.Event["id"].(string)
		if toolID == "" {
			return
		}
		if h.currentToolInputID == toolID {
			h.currentToolInputID = ""
		}
		name, ok := h.toolInputNames[toolID]
		if !ok || name == "" {
			delete(h.toolInputBuffers, toolID)
			delete(h.toolInputHadDelta, toolID)
			delete(h.toolInputNames, toolID)
			return
		}
		inputStr := ""
		if buf, ok := h.toolInputBuffers[toolID]; ok {
			inputStr = strings.TrimSpace(buf.String())
		}
		if h.isStream && (h.toolCallMode == "proxy" || h.toolCallMode == "auto") {
			if inputStr != "" {
				h.addOutputTokens(inputStr)
			}
			if idx, ok := h.toolBlocks[toolID]; ok {
				if !h.toolInputHadDelta[toolID] && inputStr != "" {
					deltaMap := perf.AcquireMap()
					deltaMap["type"] = "content_block_delta"
					deltaMap["index"] = idx
					deltaContent := perf.AcquireMap()
					deltaContent["type"] = "input_json_delta"
					deltaContent["partial_json"] = inputStr
					deltaMap["delta"] = deltaContent
					deltaData, _ := json.Marshal(deltaMap)
					perf.ReleaseMap(deltaContent)
					perf.ReleaseMap(deltaMap)
					h.writeSSE("content_block_delta", string(deltaData))
				}
				stopMap := perf.AcquireMap()
				stopMap["type"] = "content_block_stop"
				stopMap["index"] = idx
				stopData, _ := json.Marshal(stopMap)
				perf.ReleaseMap(stopMap)
				h.writeSSE("content_block_stop", string(stopData))
				delete(h.toolBlocks, toolID)
			}
		}
		delete(h.toolInputBuffers, toolID)
		delete(h.toolInputHadDelta, toolID)
		delete(h.toolInputNames, toolID)
		if h.toolCallHandled[toolID] {
			return
		}
		resolvedName := mapOrchidsToolName(name, inputStr, h.allowedIndex, h.allowedTools)
		finalName, ok := h.resolveToolName(resolvedName)
		if !ok {
			return
		}
		call := toolCall{id: toolID, name: finalName, input: inputStr}
		h.toolCallHandled[toolID] = true
		if h.toolCallMode == "proxy" && h.isStream {
			return
		}
		h.handleToolCall(call)

	case "model.tool-call":
		toolID, _ := msg.Event["toolCallId"].(string)
		toolName, _ := msg.Event["toolName"].(string)
		inputStr, _ := msg.Event["input"].(string)
		if toolID == "" {
			return
		}
		if h.currentToolInputID != "" && toolID != h.currentToolInputID {
			return
		}
		if h.toolCallHandled[toolID] {
			return
		}
		if _, ok := h.toolInputBuffers[toolID]; ok {
			return
		}
		mappedName := mapOrchidsToolName(toolName, inputStr, h.allowedIndex, h.allowedTools)
		resolvedName, ok := h.resolveToolName(mappedName)
		if !ok {
			return
		}
		call := toolCall{id: toolID, name: resolvedName, input: inputStr}
		h.toolCallHandled[toolID] = true
		h.handleToolCall(call)

	case "model.tokens-used":
		usage := msg.Event
		inputTokens, hasIn := getUsageInt(usage, "inputTokens")
		outputTokens, hasOut := getUsageInt(usage, "outputTokens")
		if !hasIn {
			inputTokens, hasIn = getUsageInt(usage, "input_tokens")
		}
		if !hasOut {
			outputTokens, hasOut = getUsageInt(usage, "output_tokens")
		}
		if hasIn || hasOut {
			in := -1
			out := -1
			if hasIn {
				in = inputTokens
			}
			if hasOut {
				out = outputTokens
			}
			h.setUsageTokens(in, out)
		}
		return

	case "model.finish":
		stopReason := "end_turn"
		if usage, ok := msg.Event["usage"].(map[string]interface{}); ok {
			inputTokens, hasIn := getUsageInt(usage, "inputTokens")
			outputTokens, hasOut := getUsageInt(usage, "outputTokens")
			if !hasIn {
				inputTokens, hasIn = getUsageInt(usage, "input_tokens")
			}
			if !hasOut {
				outputTokens, hasOut = getUsageInt(usage, "output_tokens")
			}
			if hasIn || hasOut {
				in := -1
				out := -1
				if hasIn {
					in = inputTokens
				}
				if hasOut {
					out = outputTokens
				}
				h.setUsageTokens(in, out)
			}
		}
		if finishReason, ok := msg.Event["finishReason"].(string); ok {
			switch finishReason {
			case "tool-calls", "tool_use":
				stopReason = "tool_use"
			case "stop", "end_turn":
				stopReason = "end_turn"
			}
		}

		// Force stopReason to tool_use if we have pending auto calls
		h.mu.Lock()
		if len(h.autoPendingCalls) > 0 {
			stopReason = "tool_use"
		}
		h.mu.Unlock()

		h.processAutoPendingCalls()

		if (h.toolCallMode == "internal" || h.toolCallMode == "auto") && (stopReason == "tool_use" || len(h.internalToolResults) > 0) {
			h.internalNeedsFollowup = true
			h.closeActiveBlock()
			// Return without calling finishResponse to keep the stream open for the next turn
			return
		}

		h.closeActiveBlock()
		h.finishResponse(stopReason)
	}
}
