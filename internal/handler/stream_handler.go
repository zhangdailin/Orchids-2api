package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/perf"
	"orchids-api/internal/tiktoken"
)

type streamHandler struct {
	// Configuration
	config           *config.Config
	isStream         bool
	toolCallMode     string
	suppressThinking bool
	useUpstreamUsage bool
	outputTokenMode  string

	// HTTP Response
	w       http.ResponseWriter
	flusher http.Flusher

	// State
	mu              sync.Mutex
	outputMu        sync.Mutex
	blockIndex      int
	msgID           string
	startTime       time.Time
	hasReturn       bool
	finalStopReason string
	outputTokens    int
	inputTokens     int

	// Buffers and Builders
	responseText      *strings.Builder
	outputBuilder     *strings.Builder
	textBlockBuilders map[int]*strings.Builder
	contentBlocks     []map[string]interface{}
	currentTextIndex  int

	// Tool Handling
	toolBlocks         map[string]int
	pendingToolCalls   []toolCall
	toolInputNames     map[string]string
	toolInputBuffers   map[string]*strings.Builder
	toolInputHadDelta  map[string]bool
	toolCallHandled    map[string]bool
	currentToolInputID string
	toolCallCount      int

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

		blockIndex:        -1,
		toolBlocks:        make(map[string]int),
		responseText:      perf.AcquireStringBuilder(),
		outputBuilder:     perf.AcquireStringBuilder(),
		textBlockBuilders: make(map[int]*strings.Builder),
		toolInputNames:    make(map[string]string),
		toolInputBuffers:  make(map[string]*strings.Builder),
		toolInputHadDelta: make(map[string]bool),
		toolCallHandled:   make(map[string]bool),
		msgID:             fmt.Sprintf("msg_%d", time.Now().UnixMilli()),
		startTime:         time.Now(),
		currentTextIndex:  -1,
	}
	return h
}

func (h *streamHandler) release() {
	perf.ReleaseStringBuilder(h.responseText)
	perf.ReleaseStringBuilder(h.outputBuilder)
	for _, sb := range h.textBlockBuilders {
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
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}

	h.logger.LogOutputSSE(event, data)
}

func (h *streamHandler) writeFinalSSE(event, data string) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
	if h.flusher != nil {
		h.flusher.Flush()
	}

	h.logger.LogOutputSSE(event, data)
}

func (h *streamHandler) addOutputTokens(text string) {
	if text == "" {
		return
	}
	h.outputMu.Lock()
	if h.useUpstreamUsage {
		h.outputMu.Unlock()
		return
	}
	h.outputMu.Unlock()

	if h.outputTokenMode == "stream" {
		tokens := tiktoken.EstimateTextTokens(text)
		h.outputMu.Lock()
		h.outputTokens += tokens
		h.outputMu.Unlock()
		return
	}

	h.outputMu.Lock()
	h.outputBuilder.WriteString(text)
	h.outputMu.Unlock()
}

func (h *streamHandler) finalizeOutputTokens() {
	if h.outputTokenMode == "stream" {
		return
	}
	h.outputMu.Lock()
	if h.useUpstreamUsage {
		h.outputMu.Unlock()
		return
	}
	text := h.outputBuilder.String()
	h.outputTokens = tiktoken.EstimateTextTokens(text)
	h.outputMu.Unlock()
}

func (h *streamHandler) setUsageTokens(input, output int) {
	h.outputMu.Lock()
	if input >= 0 {
		h.inputTokens = input
	}
	if output >= 0 {
		h.outputTokens = output
	}
	h.useUpstreamUsage = true
	h.outputMu.Unlock()
}

func (h *streamHandler) resetRoundState() {
	h.blockIndex = -1
	h.toolBlocks = make(map[string]int)
	h.responseText.Reset()
	h.contentBlocks = nil
	h.currentTextIndex = -1

	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	h.textBlockBuilders = map[int]*strings.Builder{}

	h.pendingToolCalls = nil
	h.toolInputNames = map[string]string{}

	for _, sb := range h.toolInputBuffers {
		perf.ReleaseStringBuilder(sb)
	}
	h.toolInputBuffers = map[string]*strings.Builder{}

	h.toolInputHadDelta = map[string]bool{}
	h.toolCallHandled = map[string]bool{}
	h.currentToolInputID = ""
	h.toolCallCount = 0
	h.outputTokens = 0
	h.outputBuilder.Reset()
	h.useUpstreamUsage = false
	h.finalStopReason = ""
}

func (h *streamHandler) shouldEmitToolCalls(stopReason string) bool {
	switch h.toolCallMode {
	case "proxy":
		return true
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

	startData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    call.id,
			"name":  call.name,
			"input": map[string]interface{}{},
		},
	})
	write("content_block_start", string(startData))

	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": fixedInput,
		},
	})
	write("content_block_delta", string(deltaData))

	stopData, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_stop",
		"index": idx,
	})
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
		deltaData, _ := json.Marshal(map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]string{"stop_reason": stopReason},
			"usage": map[string]int{"output_tokens": h.outputTokens},
		})
		h.writeFinalSSE("message_delta", string(deltaData))

		stopData, _ := json.Marshal(map[string]string{"type": "message_stop"})
		h.writeFinalSSE("message_stop", string(stopData))
	} else {
		h.flushPendingToolCalls(stopReason, h.writeFinalSSE)
		h.overrideWithLocalContext()
		h.finalizeOutputTokens()
	}

	// 记录摘要
	h.logger.LogSummary(h.inputTokens, h.outputTokens, time.Since(h.startTime), stopReason)
	slog.Info("Request completed", "input_tokens", h.inputTokens, "output_tokens", h.outputTokens, "duration", time.Since(h.startTime))
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

// Event Handlers

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
			// Emit tool call to stream for visibility
			idx := -1
			h.mu.Lock()
			if value, exists := h.toolBlocks[call.id]; exists {
				idx = value
				delete(h.toolBlocks, call.id)
			}
			h.mu.Unlock()
			h.emitToolCallStream(call, idx, h.writeSSE)
		}
		result := executeToolCall(call, h.config)
		h.internalToolResults = append(h.internalToolResults, result)
		return
	}
	h.mu.Lock()
	h.toolCallCount++
	h.mu.Unlock()
	if h.toolCallMode != "proxy" {
		h.mu.Lock()
		h.pendingToolCalls = append(h.pendingToolCalls, call)
		h.mu.Unlock()
		return
	}

	if !h.isStream {
		h.emitToolCallNonStream(call)
		return
	}

	idx := -1
	h.mu.Lock()
	if value, exists := h.toolBlocks[call.id]; exists {
		idx = value
		delete(h.toolBlocks, call.id)
	}
	h.mu.Unlock()
	h.emitToolCallStream(call, idx, h.writeSSE)
}

func (h *streamHandler) handleMessage(msg client.SSEMessage) {
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
		h.mu.Lock()
		h.blockIndex++
		idx := h.blockIndex
		h.mu.Unlock()
		data, _ := json.Marshal(map[string]interface{}{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
		h.writeSSE("content_block_start", string(data))

	case "model.reasoning-delta":
		h.mu.Lock()
		idx := h.blockIndex
		h.mu.Unlock()
		delta, _ := msg.Event["delta"].(string)
		if h.isStream {
			h.addOutputTokens(delta)
		}
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
		})
		h.writeSSE("content_block_delta", string(data))

	case "model.reasoning-end":
		h.mu.Lock()
		idx := h.blockIndex
		h.mu.Unlock()
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		})
		h.writeSSE("content_block_stop", string(data))

	case "model.text-start":
		h.mu.Lock()
		h.blockIndex++
		idx := h.blockIndex
		h.mu.Unlock()
		if !h.isStream {
			h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
				"type": "text",
			})
			h.currentTextIndex = len(h.contentBlocks) - 1
			h.textBlockBuilders[h.currentTextIndex] = &strings.Builder{}
		}
		data, _ := json.Marshal(map[string]interface{}{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		h.writeSSE("content_block_start", string(data))

	case "model.text-delta":
		h.mu.Lock()
		idx := h.blockIndex
		h.mu.Unlock()
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
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]string{"type": "text_delta", "text": delta},
		})
		h.writeSSE("content_block_delta", string(data))

	case "model.text-end":
		h.mu.Lock()
		idx := h.blockIndex
		h.mu.Unlock()
		data, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		})
		h.writeSSE("content_block_stop", string(data))

	case "model.tool-input-start":
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
		startData, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    toolID,
				"name":  finalName,
				"input": map[string]interface{}{},
			},
		})
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
		deltaData, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": delta,
			},
		})
		h.writeSSE("content_block_delta", string(deltaData))

	case "coding_agent.Edit.edit.started":
		h.editFilePath = ""
		h.editNewString = ""
		if data, ok := msg.Raw["data"].(map[string]interface{}); ok {
			if v, ok := data["file_path"].(string); ok {
				h.editFilePath = v
			}
		}

	case "coding_agent.Edit.edit.chunk":
		if data, ok := msg.Raw["data"].(map[string]interface{}); ok {
			if v, ok := data["text"].(string); ok {
				h.editNewString += v
			}
		}

	case "coding_agent.Edit.edit.completed":
		if h.editFilePath == "" {
			return
		}

		input := map[string]interface{}{
			"file_path":  h.editFilePath,
			"new_string": h.editNewString,
			// old_string is optional/not available in stream often, or we ignore it for simple notification
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			inputJSON = []byte("{}")
		}

		toolID := "toolu_edit_" + fmt.Sprintf("%d", time.Now().UnixNano()) // Simple ID generation

		call := toolCall{
			id:    toolID,
			name:  "Edit",
			input: string(inputJSON),
		}

		if h.toolCallMode == "auto" {
			// In auto mode, we just emit the visualization since the actual action is happening upstream
			// We treat this as a completed tool call notification
			if h.isStream {
				idx := -1
				h.mu.Lock()
				h.blockIndex++
				idx = h.blockIndex
				h.mu.Unlock()
				h.emitToolCallStream(call, idx, h.writeSSE)
			}
		}

		h.editFilePath = ""
		h.editNewString = ""

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
					deltaData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": inputStr,
						},
					})
					h.writeSSE("content_block_delta", string(deltaData))
				}
				stopData, _ := json.Marshal(map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
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
			case "tool-calls":
				stopReason = "tool_use"
			case "stop", "end_turn":
				stopReason = "end_turn"
			}
		}
		if stopReason == "tool_use" && (h.toolCallMode == "internal" || h.toolCallMode == "auto") {
			if len(h.internalToolResults) > 0 {
				h.internalNeedsFollowup = true
			}
			return
		}
		h.finishResponse(stopReason)
	}
}
