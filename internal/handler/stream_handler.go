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
	toolCallDedup      map[string]struct{}
	toolResultDedup    map[string]struct{}
	introDedup         map[string]struct{}
	autoToolCallSeen   bool
	toolResultCache    map[string]safeToolResult
	toolCacheEnabled   bool
	toolCacheHit       bool

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

	lastInitMsg string
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
		toolCallDedup:            make(map[string]struct{}),
		toolResultDedup:          make(map[string]struct{}),
		introDedup:               make(map[string]struct{}),
		toolResultCache:          make(map[string]safeToolResult),
		toolCacheEnabled:         toolCacheEnabled,
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
	bytes, ok := adapter.BuildOpenAIChunk(h.msgID, h.startTime.Unix(), event, []byte(data))
	if !ok {
		return
	}
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

	if h.responseFormat == adapter.FormatOpenAI {
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
	h.currentToolInputID = ""
	h.toolCallCount = 0
	h.autoPendingCalls = nil
	clear(h.toolCallDedup)
	clear(h.toolResultDedup)
	h.autoToolCallSeen = false
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
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type": "thinking",
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeThinkingBlockIndex = internalIdx
		h.activeThinkingSSEIndex = sseIdx
		h.thinkingBlockBuilders[internalIdx] = perf.AcquireStringBuilder()

		m := perf.AcquireMap()
		m["type"] = "content_block_start"
		m["index"] = sseIdx

		cb := perf.AcquireMap()
		cb["type"] = "thinking"
		cb["thinking"] = ""
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
		h.writeOpenAISSE(event, data)
		return
	}
	fmt.Fprintf(h.w, "event: %s\ndata: %s\n\n", event, data)
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
		res := executeToolCall(call, h.config)
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

	h.emitAutoToolResults(results)
}

func (h *streamHandler) emitAutoToolResults(results []safeToolResult) {
}

func (h *streamHandler) emitAutoToolResult(result safeToolResult) {
}

func (h *streamHandler) emitAutoNotice(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	note := "\n" + text + "\n"
	h.addOutputTokens(note)
	if h.isStream {
		h.emitTextBlock(note)
		return
	}
	h.responseText.WriteString(note)
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
	if h.toolCallMode == "internal" {
		result := executeSafeTool(call)
		result.output = normalizeToolResultOutput(result.output)
		h.internalToolResults = append(h.internalToolResults, result)
		return
	}
	if h.toolCallMode == "auto" {
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

func (h *streamHandler) shouldSuppressAutoText() bool {
	if h.toolCallMode != "auto" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.autoToolCallSeen
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

	case "model.reasoning-delta", "coding_agent.reasoning.chunk":
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
			return
		}

		h.mu.Lock()
		sseIdx := h.activeThinkingSSEIndex
		internalIdx := h.activeThinkingBlockIndex
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
		// Removed shouldSuppressAutoText check as it was causing empty results in some cases
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

	case "coding_agent.start", "coding_agent.initializing":
		idx := h.ensureBlock("thinking") // Trigger/Maintain thinking state
		if data, ok := msg.Event["data"].(map[string]interface{}); ok {
			if message, ok := data["message"].(string); ok {
				if h.lastInitMsg == message {
					return
				}
				h.lastInitMsg = message
				// Send as thinking delta to keep block open and visible
				deltaMap := perf.AcquireMap()
				deltaMap["type"] = "content_block_delta"
				deltaMap["index"] = idx
				deltaContent := perf.AcquireMap()
				deltaContent["type"] = "thinking_delta"
				deltaContent["thinking"] = fmt.Sprintf("\n[System: %s]\n", message)
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

		opID, _ := op["id"].(string)
		opName, _ := op["operation"].(string)

		// Map orchids fs operation to tool result
		if opID != "" {
			h.mu.Lock()
			h.internalToolResults = append(h.internalToolResults, safeToolResult{
				call: toolCall{
					id:    opID,
					name:  "fs_" + opName, // Synthetic name or mapped name
					input: "",             // Input is in op map, maybe serialize it?
				},
				// input: op, // store.Account incompatible type if safeToolResult uses map[string]interface{} for Input?
				// Let's check safeToolResult struct definition if needed.
				// Assuming it's compatible or we serialize.
				// The commented code used `input: op`.
				// Let's assume input needs to be compatible with what handler.go expects.
				input:   stringifyToolInput(op),
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
		if !h.isStream || (h.toolCallMode != "proxy" && h.toolCallMode != "auto") {
			return
		}
		h.addOutputTokens(finalName)
		h.mu.Lock()
		h.blockIndex++
		idx := h.blockIndex
		h.toolBlocks[toolID] = idx
		if h.toolCallMode == "proxy" {
			h.toolCallCount++
		}
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
		if !ok || orchids.DefaultToolMapper.IsBlocked(finalName) {
			h.toolCallHandled[toolID] = true
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
		if !ok || orchids.DefaultToolMapper.IsBlocked(resolvedName) {
			h.toolCallHandled[toolID] = true
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

		h.mu.Lock()
		hadToolCalls := h.toolCallCount > 0 || len(h.pendingToolCalls) > 0 || len(h.autoPendingCalls) > 0 || len(h.internalToolResults) > 0
		h.mu.Unlock()

		// Force stopReason to tool_use if we have pending auto calls
		h.mu.Lock()
		if len(h.autoPendingCalls) > 0 {
			stopReason = "tool_use"
		}
		h.mu.Unlock()

		h.processAutoPendingCalls()

		h.mu.Lock()
		hasToolCalls := hadToolCalls || len(h.internalToolResults) > 0
		h.mu.Unlock()

		// If upstream claims tool_use but we didn't actually handle any tool calls, treat as end_turn.
		if stopReason == "tool_use" && !hasToolCalls {
			stopReason = "end_turn"
		}

		if (h.toolCallMode == "internal" || h.toolCallMode == "auto") && (stopReason == "tool_use" || len(h.internalToolResults) > 0) {
			if hasToolCalls {
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
