package handler

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/perf"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type streamHandler struct {
	// Configuration
	config           *config.Config
	workdir          string
	isStream         bool
	toolCallMode     string
	suppressThinking bool
	useUpstreamUsage bool
	outputTokenMode  string
	responseFormat   adapter.ResponseFormat

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
	activeThinkingSSEIndex   int
	activeTextBlockIndex     int
	activeTextSSEIndex       int
	activeBlockType          string // "thinking", "text", "tool_use"

	// Buffers and Builders
	responseText          *strings.Builder
	outputBuilder         *strings.Builder
	textBlockBuilders     map[int]*strings.Builder
	thinkingBlockBuilders map[int]*strings.Builder
	thinkingBlockSigs     map[int]string
	contentBlocks         []map[string]interface{}
	currentTextIndex      int
	pendingThinkingSig    string

	// Tool Handling
	toolBlocks             map[string]int
	pendingToolCalls       []toolCall
	toolInputNames         map[string]string
	toolInputBuffers       map[string]*strings.Builder
	toolInputHadDelta      map[string]bool
	toolCallHandled        map[string]bool
	toolCallEmitted        map[string]struct{}
	currentToolInputID     string
	toolCallCount          int
	autoPendingCalls       []toolCall
	toolCallDedup          map[string]struct{}
	toolResultDedup        map[string]struct{}
	introDedup             map[string]struct{}
	autoToolCallSeen       bool
	autoToolUsePassthrough bool
	toolResultCache        map[string]safeToolResult
	toolCacheEnabled       bool
	toolCacheHit           bool
	toolReadFiles          map[string]struct{}

	// Internal Tools & Resolution
	allowedTools          map[string]string
	allowedIndex          []toolNameInfo
	hasToolList           bool
	internalToolResults   []safeToolResult
	internalNeedsFollowup bool
	preflightResults      []safeToolResult
	shouldLocalFallback   bool

	// Throttling
	lastScanTime time.Time

	// Logger
	logger *debug.Logger

	lastInitMsg       string
	finalStopSequence string
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
	responseFormat adapter.ResponseFormat,
	toolCacheEnabled bool,
	workdir string,
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
		workdir:             workdir,
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
		thinkingBlockSigs:        make(map[int]string),
		toolInputNames:           make(map[string]string),
		toolInputBuffers:         make(map[string]*strings.Builder),
		toolInputHadDelta:        make(map[string]bool),
		toolCallHandled:          make(map[string]bool),
		toolCallEmitted:          make(map[string]struct{}),
		toolCallDedup:            make(map[string]struct{}),
		toolResultDedup:          make(map[string]struct{}),
		introDedup:               make(map[string]struct{}),
		toolResultCache:          make(map[string]safeToolResult),
		toolCacheEnabled:         toolCacheEnabled,
		toolReadFiles:            make(map[string]struct{}),
		msgID:                    fmt.Sprintf("msg_%d", time.Now().UnixMilli()),
		startTime:                time.Now(),
		currentTextIndex:         -1,
		activeThinkingBlockIndex: -1,
		activeThinkingSSEIndex:   -1,
		activeTextBlockIndex:     -1,
		activeTextSSEIndex:       -1,
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
	if h.responseFormat == adapter.FormatOpenAI {
		if err := h.writeOpenAISSE(event, data); err != nil {
			h.markWriteErrorLocked(event, err)
		}
		return
	}

	if _, err := fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	if h.flusher != nil {
		h.flusher.Flush()
	}

	h.logger.LogOutputSSE(event, data)
}

func (h *streamHandler) writeOpenAISSE(event, data string) error {
	bytes, ok := adapter.BuildOpenAIChunk(h.msgID, h.startTime.Unix(), event, []byte(data))
	if !ok {
		return nil
	}
	if _, err := fmt.Fprintf(h.w, "data: %s\n\n", string(bytes)); err != nil {
		return err
	}
	if h.flusher != nil {
		h.flusher.Flush()
	}
	return nil
}

func (h *streamHandler) writeFinalSSE(event, data string) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.responseFormat == adapter.FormatOpenAI {
		if err := h.writeOpenAISSE(event, data); err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		// Send [DONE] at the very end
		if event == "message_stop" {
			if _, err := fmt.Fprintf(h.w, "data: [DONE]\n\n"); err != nil {
				h.markWriteErrorLocked(event, err)
				return
			}
			if h.flusher != nil {
				h.flusher.Flush()
			}
		}
		return
	}

	if _, err := fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
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
	if h.hasReturn {
		return
	}
	if _, err := fmt.Fprintf(h.w, ": keep-alive\n\n"); err != nil {
		h.markWriteErrorLocked("keep-alive", err)
		return
	}
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

	// 不要在这里重置 h.blockIndex。
	// 保留索引递增可避免重试时索引回绕，导致客户端报错 "Mismatched content block type"。

	h.activeThinkingBlockIndex = -1
	h.activeThinkingSSEIndex = -1
	h.activeTextBlockIndex = -1
	h.activeTextSSEIndex = -1
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
	clear(h.toolCallEmitted)
	h.currentToolInputID = ""
	h.toolCallCount = 0
	h.autoPendingCalls = nil
	clear(h.toolCallDedup)
	clear(h.toolResultDedup)
	clear(h.toolReadFiles)
	h.autoToolCallSeen = false
	h.autoToolUsePassthrough = false
	h.toolCacheHit = false
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
	fixedInput := fixToolInputForName(call.name, call.input)
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
	fixedInput := fixToolInputForName(call.name, call.input)

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

// emitToolUseFromInput 在工具输入结束时一次性输出 tool_use，避免无后续 tool_result 的悬挂调用
func (h *streamHandler) emitToolUseFromInput(toolID, toolName, inputStr string) {
	if toolID == "" || toolName == "" {
		return
	}
	if _, ok := h.toolCallEmitted[toolID]; ok {
		return
	}
	if h.toolCallMode == "auto" && h.isStream {
		if h.autoToolUsePassthrough && len(h.toolCallEmitted) > 0 {
			// 自动模式仅透传一次 tool_use，避免多工具导致客户端报错
			return
		}
	}
	h.toolCallEmitted[toolID] = struct{}{}
	if h.toolCallMode == "auto" && h.isStream {
		// 自动模式下已向客户端暴露 tool_use，本轮不再内部跟随
		h.autoToolUsePassthrough = true
	}

	if h.toolCallMode == "proxy" {
		h.mu.Lock()
		h.toolCallCount++
		h.mu.Unlock()
	}

	h.addOutputTokens(toolName)
	fixedInput := fixToolInputForName(toolName, inputStr)
	if fixedInput == "" {
		fixedInput = "{}"
	}
	if h.toolCallMode == "auto" && h.isStream && strings.EqualFold(toolName, "Read") {
		// 远程模式下不做本机路径探测，保持原始 Read
	}

	h.mu.Lock()
	h.blockIndex++
	idx := h.blockIndex
	h.mu.Unlock()

	startMap := perf.AcquireMap()
	startMap["type"] = "content_block_start"
	startMap["index"] = idx
	startContent := perf.AcquireMap()
	startContent["type"] = "tool_use"
	startContent["id"] = toolID
	startContent["name"] = toolName
	startInput := perf.AcquireMap()
	startContent["input"] = startInput
	startMap["content_block"] = startContent
	startData, _ := json.Marshal(startMap)
	perf.ReleaseMap(startInput)
	perf.ReleaseMap(startContent)
	perf.ReleaseMap(startMap)
	h.writeSSE("content_block_start", string(startData))

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
	h.writeSSE("content_block_delta", string(deltaData))

	stopMap := perf.AcquireMap()
	stopMap["type"] = "content_block_stop"
	stopMap["index"] = idx
	stopData, _ := json.Marshal(stopMap)
	perf.ReleaseMap(stopMap)
	h.writeSSE("content_block_stop", string(stopData))
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
		hasToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
			len(h.autoPendingCalls) > 0 ||
			len(h.toolCallEmitted) > 0
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
		h.mu.Lock()
		h.closeActiveBlockLocked()
		h.mu.Unlock()
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
		deltaData, err := json.Marshal(deltaMap)
		if err != nil {
			slog.Error("Failed to marshal message_delta", "error", err)
		} else {
			h.writeFinalSSE("message_delta", string(deltaData))
		}
		perf.ReleaseMap(deltaUsage)
		perf.ReleaseMap(deltaDelta)
		perf.ReleaseMap(deltaMap)

		stopMap := perf.AcquireMap()
		stopMap["type"] = "message_stop"
		stopData, err := json.Marshal(stopMap)
		if err != nil {
			slog.Error("Failed to marshal message_stop", "error", err)
		} else {
			h.writeFinalSSE("message_stop", string(stopData))
		}
		perf.ReleaseMap(stopMap)
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
	if h.config != nil && h.config.DisableToolFilter {
		return name, true
	}
	if !h.hasToolList {
		if h.toolCallMode == "internal" || h.toolCallMode == "auto" {
			return "", false
		}
		return name, true
	}
	if resolved, ok := h.allowedTools[strings.ToLower(name)]; ok {
		return resolved, true
	}
	return "", false
}

func (h *streamHandler) ensureBlock(blockType string) int {
	if blockType == "thinking" && h.suppressThinking {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// If already in a block of a different type, close it
	if h.activeBlockType != "" && h.activeBlockType != blockType {
		h.closeActiveBlockLocked()
	}

	// If already in the correct block type, return current index
	if h.activeBlockType == blockType {
		if blockType == "thinking" {
			return h.activeThinkingSSEIndex
		}
		if blockType == "text" {
			return h.activeTextSSEIndex
		}
	}

	// Start new block
	h.blockIndex++
	sseIdx := h.blockIndex
	h.activeBlockType = blockType

	var startData []byte
	switch blockType {
	case "thinking":
		signature := h.pendingThinkingSig
		h.pendingThinkingSig = ""
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type":      "thinking",
			"signature": signature,
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeThinkingBlockIndex = internalIdx
		h.activeThinkingSSEIndex = sseIdx
		h.thinkingBlockBuilders[internalIdx] = perf.AcquireStringBuilder()
		h.thinkingBlockSigs[internalIdx] = signature

		m := perf.AcquireMap()
		m["type"] = "content_block_start"
		m["index"] = sseIdx

		cb := perf.AcquireMap()
		cb["type"] = "thinking"
		cb["thinking"] = ""
		cb["signature"] = signature
		m["content_block"] = cb

		startData, _ = json.Marshal(m)
		perf.ReleaseMap(cb)
		perf.ReleaseMap(m)
	case "text":
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type": "text",
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeTextBlockIndex = internalIdx
		h.activeTextSSEIndex = sseIdx
		h.textBlockBuilders[internalIdx] = perf.AcquireStringBuilder()

		m := perf.AcquireMap()
		m["type"] = "content_block_start"
		m["index"] = sseIdx

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

	return sseIdx
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

	var sseIdx int
	switch h.activeBlockType {
	case "thinking":
		sseIdx = h.activeThinkingSSEIndex
		h.activeThinkingBlockIndex = -1
		h.activeThinkingSSEIndex = -1
	case "text":
		sseIdx = h.activeTextSSEIndex
		h.activeTextBlockIndex = -1
		h.activeTextSSEIndex = -1
	default:
		// tool_use and others are usually handled as single-event blocks or managed separately
		h.activeBlockType = ""
		return
	}

	h.activeBlockType = ""

	m := perf.AcquireMap()
	m["type"] = "content_block_stop"
	m["index"] = sseIdx
	stopData, err := json.Marshal(m)
	if err != nil {
		slog.Error("Failed to marshal content_block_stop", "error", err)
	}
	perf.ReleaseMap(m)

	h.writeSSELocked("content_block_stop", string(stopData))
}

func (h *streamHandler) writeSSELocked(event, data string) {
	if !h.isStream {
		return
	}
	if h.hasReturn {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		if err := h.writeOpenAISSE(event, data); err != nil {
			h.markWriteErrorLocked(event, err)
		}
		return
	}
	if _, err := fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	if h.flusher != nil {
		h.flusher.Flush()
	}
	h.logger.LogOutputSSE(event, data)
	// Log to slog only when debug enabled
	if h.config != nil && h.config.DebugEnabled {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
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
		call := calls[i]
		mutating := isMutatingTool(call.name)
		if h.toolCacheEnabled && !mutating {
			if key := h.toolCacheKey(call); key != "" {
				if cached, ok := h.loadToolCache(key); ok {
					results[i] = cached
					h.markToolCacheHit()
					return
				}
			}
		}
		slog.Debug("Executing tool", "name", call.name, "workdir", h.workdir)
		res := executeToolCallWithBaseDir(call, h.config, h.workdir)
		res.output = normalizeToolResultOutput(res.output)
		if h.toolCacheEnabled {
			if mutating {
				h.clearToolCache()
			} else if key := h.toolCacheKey(call); key != "" {
				h.storeToolCache(key, res)
			}
		}
		results[i] = res
	})

	// Append results in order
	for _, res := range results {
		h.internalToolResults = append(h.internalToolResults, res)
	}
}

func normalizeToolResultOutput(output string) string {
	if output == "" {
		return ""
	}
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.TrimSpace(output)
	return output
}

func isMutatingTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	name = strings.ToLower(orchids.NormalizeToolName(name))
	switch name {
	case "write", "edit", "bash":
		return true
	default:
		return false
	}
}

func (h *streamHandler) toolCacheKey(call toolCall) string {
	return hashToolCallKey(call.name, normalizeToolDedupInput(call.input))
}

func (h *streamHandler) loadToolCache(key string) (safeToolResult, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	res, ok := h.toolResultCache[key]
	return res, ok
}

func (h *streamHandler) clearToolCache() {
	h.mu.Lock()
	if len(h.toolResultCache) > 0 {
		clear(h.toolResultCache)
	}
	h.mu.Unlock()
}

func (h *streamHandler) storeToolCache(key string, res safeToolResult) {
	h.mu.Lock()
	h.toolResultCache[key] = res
	h.mu.Unlock()
}

func (h *streamHandler) markToolCacheHit() {
	h.mu.Lock()
	h.toolCacheHit = true
	h.mu.Unlock()
}

func (h *streamHandler) hadToolCacheHit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolCacheHit
}

func (h *streamHandler) shouldEmitToolResult(result safeToolResult) bool {
	if h.toolCallMode != "auto" {
		return true
	}
	key := strings.TrimSpace(result.call.id)
	if key == "" {
		key = hashToolCallKey(result.call.name, normalizeToolDedupInput(result.call.input))
	}
	if key == "" {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.toolResultDedup[key]; ok {
		return false
	}
	h.toolResultDedup[key] = struct{}{}
	return true
}

func normalizeToolDedupInput(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	fixed := strings.TrimSpace(fixToolInput(input))
	if fixed == "" {
		return input
	}
	return fixed
}

func hashToolCallKey(name, input string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	if input != "" {
		_, _ = h.Write([]byte(input))
	}
	return fmt.Sprintf("call:%x", h.Sum64())
}

func (h *streamHandler) emitTextBlock(text string) {
	if !h.isStream || text == "" {
		return
	}

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
	deltaContent["text"] = text
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

func (h *streamHandler) handleToolCall(call toolCall) {
	if call.id == "" {
		return
	}
	if !h.shouldAcceptToolCall(call) {
		return
	}
	if h.enforceReadBeforeWrite(call) {
		return
	}
	h.handleToolCallAfterChecks(call)
}

func (h *streamHandler) handleToolCallAfterChecks(call toolCall) {
	if h.toolCallMode == "internal" {
		result := executeSafeTool(call)
		result.output = normalizeToolResultOutput(result.output)
		h.internalToolResults = append(h.internalToolResults, result)
		return
	}
	if h.toolCallMode == "auto" {
		if h.autoToolUsePassthrough {
			// 已向客户端输出 tool_use，本轮交由客户端处理 tool_result
			return
		}
		// 自动模式下不再输出“Running tool”提示，避免噪音
		h.mu.Lock()
		h.autoToolCallSeen = true
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

func (h *streamHandler) shouldAcceptToolCall(call toolCall) bool {
	nameKey := strings.ToLower(strings.TrimSpace(call.name))
	if nameKey == "" {
		return false
	}
	inputKey := normalizeToolDedupInput(call.input)
	callKey := hashToolCallKey(nameKey, inputKey)

	h.mu.Lock()
	defer h.mu.Unlock()

	// 同一轮只去重相同输入的调用，允许同名工具不同参数
	if callKey != "" {
		if _, ok := h.toolCallDedup[callKey]; ok {
			if h.config != nil && h.config.DebugEnabled {
				slog.Debug("duplicate tool call suppressed", "tool", call.name)
			}
			return false
		}
		h.toolCallDedup[callKey] = struct{}{}
	}
	return true
}

func (h *streamHandler) markWriteErrorLocked(event string, err error) {
	if err == nil {
		return
	}
	if h.hasReturn {
		return
	}
	h.hasReturn = true
	h.finalStopReason = "write_error"
	slog.Warn("SSE 写入失败，已终止输出", "event", event, "error", err)
}

func (h *streamHandler) forceFinishIfMissing() {
	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	hasToolCalls := h.toolCallCount > 0 ||
		len(h.pendingToolCalls) > 0 ||
		len(h.autoPendingCalls) > 0 ||
		len(h.internalToolResults) > 0 ||
		len(h.toolCallEmitted) > 0
	h.mu.Unlock()
	stopReason := "end_turn"
	if hasToolCalls {
		stopReason = "tool_use"
	}
	slog.Warn("上游未发送结束标记，强制结束响应", "stop_reason", stopReason)
	h.finishResponse(stopReason)
}

func (h *streamHandler) enforceReadBeforeWrite(call toolCall) bool {
	name := strings.ToLower(strings.TrimSpace(call.name))
	if name == "" {
		return false
	}
	if name == "read" {
		path := extractToolFilePath(call.input)
		if path != "" {
			h.mu.Lock()
			h.toolReadFiles[path] = struct{}{}
			h.mu.Unlock()
		}
		return false
	}
	if name != "write" && name != "edit" {
		return false
	}
	path := extractToolFilePath(call.input)
	if path == "" {
		return false
	}
	h.mu.Lock()
	_, ok := h.toolReadFiles[path]
	h.mu.Unlock()
	if ok {
		return false
	}
	h.emitReadBeforeWriteError(call, path)
	return true
}

func (h *streamHandler) emitReadBeforeWriteError(call toolCall, path string) {
	msg := "File has not been read yet. Read it first before writing to it."
	if h.config != nil && h.config.DebugEnabled {
		slog.Warn("拦截写入工具调用：未先读取文件", "tool", call.name, "path", path)
	}
	h.mu.Lock()
	h.internalToolResults = append(h.internalToolResults, safeToolResult{
		call:    call,
		input:   parseToolInputValue(call.input),
		output:  msg,
		isError: true,
	})
	h.internalNeedsFollowup = true
	h.toolCallHandled[call.id] = true
	h.mu.Unlock()
}

func extractToolFilePath(inputStr string) string {
	input := parseToolInputMap(inputStr)
	return toolInputString(input, "file_path", "path", "filename", "file")
}

func (h *streamHandler) shouldSkipIntroDelta(delta string) bool {
	key := normalizeIntroKey(delta)
	if key == "" {
		return false
	}
	h.mu.Lock()
	h.introDedup[key] = struct{}{}
	h.mu.Unlock()
	return true
}

func normalizeIntroKey(delta string) string {
	text := strings.TrimSpace(delta)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	switch lower {
	case "hi! how can i help you today?",
		"hello! how can i help you today?",
		"hi! how can i help you today!",
		"hello! how can i help you today!":
		return "intro:en:greet"
	}
	if strings.HasPrefix(text, "你好") || strings.HasPrefix(text, "您好") {
		return "intro:zh:greet"
	}
	if strings.HasPrefix(lower, "我是 warp") || strings.HasPrefix(lower, "我是 warp agent mode") || strings.HasPrefix(lower, "我是 warp 智能代理模式") {
		return "intro:zh:warp"
	}
	if strings.HasPrefix(lower, "我是 claude") || strings.HasPrefix(lower, "我是 claude 4.5") || strings.HasPrefix(lower, "我是 claude 4") {
		return "intro:zh:claude"
	}
	return ""
}

// extractThinkingSignature 尝试从上游事件中提取 signature（优先 event.signature，其次 event.data.signature）
func extractThinkingSignature(event map[string]interface{}) string {
	if event == nil {
		return ""
	}
	if sig, ok := event["signature"].(string); ok {
		return strings.TrimSpace(sig)
	}
	if data, ok := event["data"].(map[string]interface{}); ok {
		if sig, ok := data["signature"].(string); ok {
			return strings.TrimSpace(sig)
		}
	}
	return ""
}

func (h *streamHandler) handleMessage(msg upstream.SSEMessage) {
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

	// Instrument: Log detailed error info
	if strings.HasSuffix(eventKey, ".error") || strings.Contains(eventKey, "error") {
		if msg.Event != nil {
			if data, ok := msg.Event["data"]; ok {
				slog.Warn("SSE Error Payload", "type", eventKey, "data", data)
			}
		}
	}
	if h.suppressThinking {
		if strings.HasPrefix(eventKey, "model.reasoning-") ||
			strings.HasPrefix(eventKey, "coding_agent.reasoning") ||
			eventKey == "coding_agent.start" ||
			eventKey == "coding_agent.initializing" {
			return
		}
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
		h.pendingThinkingSig = ""
		if sig := extractThinkingSignature(msg.Event); sig != "" {
			h.pendingThinkingSig = sig
			h.ensureBlock("thinking")
		}

	case "model.reasoning-delta", "coding_agent.reasoning.chunk":
		sig := ""
		if h.pendingThinkingSig == "" {
			sig = extractThinkingSignature(msg.Event)
			if sig != "" {
				h.pendingThinkingSig = sig
			}
		} else {
			sig = h.pendingThinkingSig
		}
		delta := ""
		if msg.Type == "model" {
			delta, _ = msg.Event["delta"].(string)
		} else {
			// coding_agent.reasoning.chunk
			if data, ok := msg.Event["data"].(map[string]interface{}); ok {
				delta, _ = data["text"].(string)
			}
		}
		if delta == "" {
			if sig != "" {
				h.ensureBlock("thinking")
				h.mu.Lock()
				internalIdx := h.activeThinkingBlockIndex
				if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
					if existing, ok := h.thinkingBlockSigs[internalIdx]; ok && existing == "" {
						h.thinkingBlockSigs[internalIdx] = sig
						h.contentBlocks[internalIdx]["signature"] = sig
					}
				}
				h.mu.Unlock()
			}
			return
		}

		h.mu.Lock()
		sseIdx := h.activeThinkingSSEIndex
		internalIdx := h.activeThinkingBlockIndex
		if sig != "" && internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			if existing, ok := h.thinkingBlockSigs[internalIdx]; ok && existing == "" {
				h.thinkingBlockSigs[internalIdx] = sig
				h.contentBlocks[internalIdx]["signature"] = sig
			}
		}
		h.mu.Unlock()
		if sseIdx < 0 {
			// If we get delta but no thinking block is active, try to ensure one
			sseIdx = h.ensureBlock("thinking")
			h.mu.Lock()
			internalIdx = h.activeThinkingBlockIndex
			h.mu.Unlock()
		}
		if h.isStream {
			h.addOutputTokens(delta)
		}
		// Always update internal state for history
		h.mu.Lock()
		if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			builder, ok := h.thinkingBlockBuilders[internalIdx]
			if !ok {
				builder = perf.AcquireStringBuilder()
				h.thinkingBlockBuilders[internalIdx] = builder
			}
			builder.WriteString(delta)
		}
		h.mu.Unlock()
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = sseIdx
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

	case "model.text-delta", "coding_agent.output_text.delta":
		delta := ""
		if msg.Type == "model" {
			delta, _ = msg.Event["delta"].(string)
		} else {
			// coding_agent.output_text.delta
			delta, _ = msg.Event["delta"].(string)
		}
		if delta == "" {
			return
		}

		if h.shouldSkipIntroDelta(delta) {
			return
		}

		h.mu.Lock()
		sseIdx := h.activeTextSSEIndex
		internalIdx := h.activeTextBlockIndex
		h.mu.Unlock()
		if sseIdx < 0 {
			// If we get delta but no text block is active, try to ensure one
			sseIdx = h.ensureBlock("text")
			h.mu.Lock()
			internalIdx = h.activeTextBlockIndex
			h.mu.Unlock()
		}
		h.addOutputTokens(delta)
		if !h.isStream {
			h.responseText.WriteString(delta)
		}
		// Always update internal state for history
		h.mu.Lock()
		if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			builder, ok := h.textBlockBuilders[internalIdx]
			if !ok {
				builder = perf.AcquireStringBuilder()
				h.textBlockBuilders[internalIdx] = builder
			}
			builder.WriteString(delta)
		}
		h.mu.Unlock()
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = sseIdx
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

	case "coding_agent.start", "coding_agent.initializing", "init":
		// Ensure a thinking block is open for these status updates when we already have signature or block
		h.mu.Lock()
		hasThinkingBlock := h.activeThinkingSSEIndex >= 0
		h.mu.Unlock()
		if hasThinkingBlock || h.pendingThinkingSig != "" {
			h.ensureBlock("thinking")
		}
		if h.isStream {
			data, _ := json.Marshal(msg.Event)
			h.writeSSE(msg.Type, string(data))
		}
		return

	case "coding_agent.Write.started", "coding_agent.Edit.edit.started":
		if h.isStream {
			data, _ := msg.Event["data"].(map[string]interface{})
			path, _ := data["file_path"].(string)
			op := "Writing"
			if strings.Contains(msg.Type, "Edit") {
				op = "Editing"
			}
			h.ensureBlock("thinking")
			h.emitThinkingDelta(fmt.Sprintf("\n[%s %s...]\n", op, path))

			rawData, _ := json.Marshal(msg.Event)
			h.writeSSE(msg.Type, string(rawData))
		}
		return

	case "coding_agent.Write.content.chunk", "coding_agent.Edit.edit.chunk":
		if h.isStream {
			data, _ := msg.Event["data"].(map[string]interface{})
			text, _ := data["text"].(string)
			if text != "" {
				// Map Orchids code chunks to thinking blocks for standard UIs
				h.emitThinkingDelta(text)
			}
			rawData, _ := json.Marshal(msg.Event)
			h.writeSSE(msg.Type, string(rawData))
		}
		return

	case "coding_agent.Write.content.completed", "coding_agent.Edit.edit.completed", "coding_agent.edit_file.completed":
		if h.isStream {
			h.emitThinkingDelta("\n[Done]\n")
			data, _ := json.Marshal(msg.Event)
			h.writeSSE(msg.Type, string(data))
		}
		return

	case "fs_operation":
		// Throttle keep-alives and passthrough to avoid flooding
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
		if h.isStream {
			data, _ := json.Marshal(msg.Event)
			h.writeSSE(msg.Type, string(data))
		} else {
			h.writeKeepAlive()
		}
		return

	case "fs_operation_result":
		success, _ := msg.Event["success"].(bool)
		data := msg.Event["data"]
		errMsg, _ := msg.Event["error"].(string)
		op, _ := msg.Event["op"].(map[string]interface{})

		_ = op["id"]
		_, _ = op["operation"].(string)

		// Map orchids operation back to Claude tool name if possible
		// mappedName := client.DefaultToolMapper.FromOrchids(opName)

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

		if h.autoToolUsePassthrough {
			// 已向客户端暴露 tool_use，本轮不再注入内部 tool_result
			return
		}

		// Map orchids fs operation to tool result（对齐 AIClient-2-API）
		if call, input, ok := mapFSOperationToToolCall(op); ok {
			h.mu.Lock()
			h.internalToolResults = append(h.internalToolResults, safeToolResult{
				call:    call,
				input:   input,
				output:  output,
				isError: !success,
			})
			h.internalNeedsFollowup = true
			h.mu.Unlock()
		}

	case "model.tool-input-start":
		h.closeActiveBlock() // Tool input starts a separate block mechanism
		toolID, _ := msg.Event["id"].(string)
		toolName, _ := msg.Event["toolName"].(string)
		if toolID == "" || toolName == "" {
			return
		}
		mappedName := mapOrchidsToolName(toolName, "", h.allowedIndex, h.allowedTools)
		finalName, ok := h.resolveToolName(mappedName)
		if !ok || orchids.DefaultToolMapper.IsBlocked(finalName) {
			h.toolCallHandled[toolID] = true
			return
		}
		h.currentToolInputID = toolID
		h.toolInputNames[toolID] = toolName
		h.toolInputBuffers[toolID] = perf.AcquireStringBuilder()
		h.toolInputHadDelta[toolID] = false
		// 注意：不要在 tool-input-start 就发送 tool_use，避免上游中断导致无 tool_result。
		return

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
		delete(h.toolInputBuffers, toolID)
		delete(h.toolInputHadDelta, toolID)
		delete(h.toolInputNames, toolID)
		if h.toolCallHandled[toolID] {
			return
		}
		resolvedName := mapOrchidsToolName(name, inputStr, h.allowedIndex, h.allowedTools)
		finalName, ok := h.resolveToolName(resolvedName)
		if !ok || orchids.DefaultToolMapper.IsBlocked(finalName) {
			h.toolCallHandled[toolID] = true
			return
		}
		call := toolCall{id: toolID, name: finalName, input: inputStr}
		h.toolCallHandled[toolID] = true
		if !h.shouldAcceptToolCall(call) {
			return
		}
		if h.enforceReadBeforeWrite(call) {
			return
		}
		if h.isStream && (h.toolCallMode == "proxy" || h.toolCallMode == "auto") {
			if inputStr != "" {
				h.addOutputTokens(inputStr)
			}
			h.emitToolUseFromInput(toolID, finalName, inputStr)
		}
		if h.toolCallMode == "proxy" && h.isStream {
			return
		}
		h.handleToolCallAfterChecks(call)

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
		if !ok || orchids.DefaultToolMapper.IsBlocked(resolvedName) {
			h.toolCallHandled[toolID] = true
			return
		}
		call := toolCall{id: toolID, name: resolvedName, input: inputStr}
		h.toolCallHandled[toolID] = true
		if !h.shouldAcceptToolCall(call) {
			return
		}
		if h.enforceReadBeforeWrite(call) {
			return
		}
		if h.isStream && (h.toolCallMode == "proxy" || h.toolCallMode == "auto") {
			h.emitToolUseFromInput(toolID, resolvedName, inputStr)
		}
		h.handleToolCallAfterChecks(call)

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

		h.mu.Lock()
		toolUseEmitted := len(h.toolCallEmitted) > 0
		autoPassthrough := h.autoToolUsePassthrough
		hadToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
			len(h.autoPendingCalls) > 0 ||
			len(h.internalToolResults) > 0 ||
			toolUseEmitted
		if autoPassthrough {
			// 已输出 tool_use，不再执行内部工具或注入 tool_result
			h.autoPendingCalls = nil
			h.internalToolResults = nil
		}
		h.mu.Unlock()

		// Force stopReason to tool_use if we have pending auto calls
		h.mu.Lock()
		if len(h.autoPendingCalls) > 0 || len(h.toolCallEmitted) > 0 {
			stopReason = "tool_use"
		}
		h.mu.Unlock()

		if !autoPassthrough {
			h.processAutoPendingCalls()
		}

		h.mu.Lock()
		hasToolCalls := hadToolCalls || len(h.internalToolResults) > 0 || len(h.toolCallEmitted) > 0
		autoPassthrough = h.autoToolUsePassthrough
		h.mu.Unlock()

		// If upstream claims tool_use but we didn't actually handle any tool calls, treat as end_turn.
		if stopReason == "tool_use" && !hasToolCalls {
			stopReason = "end_turn"
		}

		if (h.toolCallMode == "internal" || h.toolCallMode == "auto") && (stopReason == "tool_use" || len(h.internalToolResults) > 0) {
			if hasToolCalls && !autoPassthrough {
				h.internalNeedsFollowup = true
				h.closeActiveBlock()
				// Return without calling finishResponse to keep the stream open for the next turn
				return
			}
		}

		h.closeActiveBlock()
		h.finishResponse(stopReason)
	}
}

func (h *streamHandler) emitThinkingDelta(delta string) {
	if delta == "" || h.suppressThinking {
		return
	}
	h.mu.Lock()
	sseIdx := h.activeThinkingSSEIndex
	internalIdx := h.activeThinkingBlockIndex
	h.mu.Unlock()

	if sseIdx < 0 {
		sseIdx = h.ensureBlock("thinking")
		h.mu.Lock()
		internalIdx = h.activeThinkingBlockIndex
		h.mu.Unlock()
	}

	h.addOutputTokens(delta)

	h.mu.Lock()
	if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
		builder, ok := h.thinkingBlockBuilders[internalIdx]
		if !ok {
			builder = perf.AcquireStringBuilder()
			h.thinkingBlockBuilders[internalIdx] = builder
		}
		builder.WriteString(delta)
	}
	h.mu.Unlock()

	m := perf.AcquireMap()
	m["type"] = "content_block_delta"
	m["index"] = sseIdx
	deltaMap := perf.AcquireMap()
	deltaMap["type"] = "thinking_delta"
	deltaMap["thinking"] = delta
	m["delta"] = deltaMap
	data, _ := json.Marshal(m)
	h.writeSSE("content_block_delta", string(data))
	perf.ReleaseMap(deltaMap)
	perf.ReleaseMap(m)
}

func stringifyToolInput(input interface{}) string {
	if input == nil {
		return "{}"
	}
	bytes, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(bytes)
}

// mapFSOperationToToolCall 将 Orchids fs_operation 转成标准工具调用
// 参考 AIClient-2-API 的映射逻辑，尽量还原到 Read/Write/Edit/Bash/Glob/Grep/LS
func mapFSOperationToToolCall(op map[string]interface{}) (toolCall, interface{}, bool) {
	if op == nil {
		return toolCall{}, nil, false
	}
	opID := opString(op, "id")
	if opID == "" {
		return toolCall{}, nil, false
	}
	opType := strings.ToLower(strings.TrimSpace(opString(op, "operation", "op", "type")))
	if opType == "" {
		call := toolCall{id: opID, name: "fs_operation", input: stringifyToolInput(op)}
		return call, op, true
	}

	toolName := ""
	input := map[string]interface{}{}
	switch opType {
	case "list", "ls":
		toolName = "LS"
		path := opString(op, "path", "dir", "file_path", "file")
		if path == "" {
			path = "."
		}
		input["path"] = path
	case "read":
		toolName = "Read"
		path := opString(op, "path", "file_path", "file")
		if path != "" {
			input["file_path"] = path
		}
	case "write":
		path := opString(op, "path", "file_path", "file")
		oldStr := opString(op, "old_string", "old_str", "old_content", "oldString")
		newStr := opString(op, "new_string", "new_str", "new_content", "newString")
		if oldStr != "" {
			toolName = "Edit"
			if path != "" {
				input["file_path"] = path
			}
			input["old_string"] = oldStr
			if newStr != "" {
				input["new_string"] = newStr
			}
		} else {
			toolName = "Write"
			if path != "" {
				input["file_path"] = path
			}
			if content, ok := opContent(op, "content", "new_content"); ok {
				input["content"] = content
			}
		}
	case "edit":
		toolName = "Edit"
		path := opString(op, "path", "file_path", "file")
		if path != "" {
			input["file_path"] = path
		}
		if edits, ok := op["edits"]; ok {
			input["edits"] = edits
		} else {
			oldStr := opString(op, "old_string", "old_str", "old_content", "oldString")
			newStr := opString(op, "new_string", "new_str", "new_content", "newString")
			if oldStr != "" {
				input["old_string"] = oldStr
			}
			if newStr != "" {
				input["new_string"] = newStr
			}
		}
	case "grep", "ripgrep", "search":
		toolName = "Grep"
		pattern := opString(op, "pattern", "query", "search")
		if pattern != "" {
			input["pattern"] = pattern
		}
		path := opString(op, "path", "dir")
		if path == "" {
			path = "."
		}
		input["path"] = path
	case "glob":
		toolName = "Glob"
		pattern := opString(op, "pattern", "query", "glob")
		if pattern == "" {
			pattern = "*"
		}
		input["pattern"] = pattern
		path := opString(op, "path", "dir")
		if path == "" {
			path = "."
		}
		input["path"] = path
	case "run_command", "execute", "bash":
		toolName = "Bash"
		if command := opString(op, "command", "cmd"); command != "" {
			input["command"] = command
		}
	case "todowrite", "todo_write":
		toolName = "TodoWrite"
		if content, ok := opContent(op, "content", "todos"); ok {
			input["content"] = content
		}
	default:
		toolName = opType
		input = op
	}

	if toolName == "" {
		toolName = "fs_operation"
	}
	if len(input) == 0 {
		input = op
	}

	call := toolCall{
		id:    opID,
		name:  toolName,
		input: stringifyToolInput(input),
	}
	return call, input, true
}

func opString(op map[string]interface{}, keys ...string) string {
	if op == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := op[key]; ok {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func opContent(op map[string]interface{}, keys ...string) (interface{}, bool) {
	if op == nil {
		return nil, false
	}
	for _, key := range keys {
		if v, ok := op[key]; ok {
			switch val := v.(type) {
			case string:
				if strings.TrimSpace(val) == "" {
					continue
				}
				return val, true
			default:
				encoded, err := json.Marshal(val)
				if err != nil {
					continue
				}
				return string(encoded), true
			}
		}
	}
	return nil, false
}
