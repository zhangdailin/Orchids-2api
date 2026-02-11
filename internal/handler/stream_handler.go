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
	"orchids-api/internal/perf"
	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
)

type streamHandler struct {
	// Configuration
	config           *config.Config
	workdir          string
	isStream         bool
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
	writeChunkBuffer      *strings.Builder
	textBlockBuilders     map[int]*strings.Builder
	thinkingBlockBuilders map[int]*strings.Builder
	thinkingBlockSigs     map[int]string
	contentBlocks         []map[string]interface{}
	currentTextIndex      int
	pendingThinkingSig    string
	hasTextOutput         bool

	// Tool Handling (proxy mode only)
	toolBlocks         map[string]int
	pendingToolCalls   []toolCall
	toolInputNames     map[string]string
	toolInputBuffers   map[string]*strings.Builder
	toolInputHadDelta  map[string]bool
	toolCallHandled    map[string]bool
	toolCallEmitted    map[string]struct{}
	currentToolInputID string
	toolCallCount      int
	bashCallDedup      map[string]struct{}
	seedToolDedup      map[string]struct{}
	toolDedupCount     int
	toolDedupKeys      map[string]int
	introDedup         map[string]struct{}

	// Throttling
	lastScanTime time.Time

	// Callbacks
	onConversationID func(string) // 上游返回 conversationID 时回调

	// Logger
	logger *debug.Logger
}

func newStreamHandler(
	cfg *config.Config,
	w http.ResponseWriter,
	logger *debug.Logger,
	suppressThinking bool,
	isStream bool,
	responseFormat adapter.ResponseFormat,
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
		config:           cfg,
		workdir:          workdir,
		w:                w,
		flusher:          flusher,
		isStream:         isStream,
		logger:           logger,
		suppressThinking: suppressThinking,
		outputTokenMode:  outputTokenMode,
		responseFormat:   responseFormat,

		blockIndex:               -1,
		toolBlocks:               make(map[string]int),
		responseText:             perf.AcquireStringBuilder(),
		outputBuilder:            perf.AcquireStringBuilder(),
		writeChunkBuffer:         perf.AcquireStringBuilder(),
		textBlockBuilders:        make(map[int]*strings.Builder),
		thinkingBlockBuilders:    make(map[int]*strings.Builder),
		thinkingBlockSigs:        make(map[int]string),
		toolInputNames:           make(map[string]string),
		toolInputBuffers:         make(map[string]*strings.Builder),
		toolInputHadDelta:        make(map[string]bool),
		toolCallHandled:          make(map[string]bool),
		toolCallEmitted:          make(map[string]struct{}),
		bashCallDedup:            make(map[string]struct{}),
		seedToolDedup:            make(map[string]struct{}),
		toolDedupKeys:            make(map[string]int),
		introDedup:               make(map[string]struct{}),
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
	perf.ReleaseStringBuilder(h.writeChunkBuffer)
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
	clear(h.bashCallDedup)
	for key := range h.seedToolDedup {
		h.bashCallDedup[key] = struct{}{}
	}
	h.toolDedupCount = 0
	clear(h.toolDedupKeys)
	h.currentToolInputID = ""
	h.toolCallCount = 0
	h.outputTokens = 0
	h.outputBuilder.Reset()
	h.writeChunkBuffer.Reset()
	h.useUpstreamUsage = false
	h.finalStopReason = ""
	h.hasTextOutput = false
}

func (h *streamHandler) shouldEmitToolCalls(stopReason string) bool {
	return true
}

// seedSideEffectDedupFromMessages 预热跨轮去重键，避免工具结果回传后的下一轮重复执行同一副作用命令。
// 仅采集“最近一条含文本用户消息之后”的 assistant tool_use，避免污染更早轮次。
func (h *streamHandler) seedSideEffectDedupFromMessages(messages []prompt.Message) {
	if len(messages) == 0 {
		return
	}
	lastUserTextIdx := -1
	for i, msg := range messages {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		if strings.TrimSpace(messagePlainText(msg.Content)) != "" {
			lastUserTextIdx = i
		}
	}
	if lastUserTextIdx < 0 {
		return
	}

	for i, msg := range messages {
		if i <= lastUserTextIdx || strings.ToLower(strings.TrimSpace(msg.Role)) != "assistant" {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_use" {
				continue
			}
			nameKey := strings.ToLower(strings.TrimSpace(block.Name))
			if nameKey == "" {
				continue
			}
			input := strings.TrimSpace(stringifyToolInput(block.Input))
			if input == "" {
				input = "{}"
			}
			key := sideEffectToolDedupKey(nameKey, input)
			if key == "" {
				continue
			}
			h.seedToolDedup[key] = struct{}{}
			h.bashCallDedup[key] = struct{}{}
		}
	}
}

func messagePlainText(content prompt.MessageContent) string {
	if content.IsString() {
		return content.GetText()
	}
	blocks := content.GetBlocks()
	if len(blocks) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(block.Text)
	}
	return sb.String()
}

func stringifyToolInput(input interface{}) string {
	switch v := input.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(raw)
	}
}

// sanitizeToolInput normalizes upstream tool input for Claude Code compatibility.
// It drops or maps fields known to cause local tool validation failures.
func sanitizeToolInput(name, input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return input
	}

	nameKey := strings.ToLower(strings.TrimSpace(name))
	changed := false
	mapField := func(from, to string) {
		v, ok := payload[from]
		if !ok {
			return
		}
		if _, exists := payload[to]; !exists {
			payload[to] = v
			changed = true
		}
		delete(payload, from)
		changed = true
	}

	switch nameKey {
	case "write":
		// Claude Code Write tool rejects unknown field "overwrite".
		if _, ok := payload["overwrite"]; ok {
			delete(payload, "overwrite")
			changed = true
		}
		mapField("path", "file_path")
	case "edit":
		mapField("path", "file_path")
	case "read":
		mapField("path", "file_path")
	case "bash":
		mapField("cmd", "command")
	}

	if !changed {
		return input
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return input
	}
	return string(normalized)
}

func (h *streamHandler) emitToolCallNonStream(call toolCall) {
	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	if inputJSON == "" {
		inputJSON = "{}"
	}
	var inputValue interface{}
	if err := json.Unmarshal([]byte(inputJSON), &inputValue); err != nil {
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
	inputJSON := strings.TrimSpace(call.input)
	if inputJSON == "" {
		inputJSON = "{}"
	}

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
	deltaContent["partial_json"] = inputJSON
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
	h.toolCallEmitted[toolID] = struct{}{}

	h.mu.Lock()
	h.toolCallCount++
	h.mu.Unlock()

	h.addOutputTokens(toolName)
	inputJSON := strings.TrimSpace(inputStr)
	if inputJSON == "" {
		inputJSON = "{}"
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
	deltaContent["partial_json"] = inputJSON
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

func (h *streamHandler) finishResponse(stopReason string) {
	if stopReason == "tool_use" {
		h.mu.Lock()
		hasToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
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
		var blockStopData string
		h.mu.Lock()
		if stopData, ok := h.popActiveBlockStopDataLocked(); ok {
			blockStopData = stopData
		}
		h.mu.Unlock()
		if blockStopData != "" {
			h.writeFinalSSE("content_block_stop", blockStopData)
		}
		if stopReason != "tool_use" {
			h.emitWriteChunkFallbackIfNeeded(h.writeFinalSSE)
		}
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
		if stopReason != "tool_use" {
			h.emitWriteChunkFallbackIfNeeded(h.writeFinalSSE)
		}
		h.flushPendingToolCalls(stopReason, h.writeFinalSSE)
		h.finalizeOutputTokens()
	}

	// 记录摘要
	h.mu.Lock()
	suppressedDedup := h.toolDedupCount
	dedupKeys := make(map[string]int, len(h.toolDedupKeys))
	for k, v := range h.toolDedupKeys {
		dedupKeys[k] = v
	}
	h.mu.Unlock()
	if suppressedDedup > 0 {
		slog.Info("tool call dedup summary", "suppressed_count", suppressedDedup, "dedup_keys", dedupKeys)
	}
	h.logger.LogSummary(h.inputTokens, h.outputTokens, time.Since(h.startTime), stopReason)
	slog.Debug("Request completed", "input_tokens", h.inputTokens, "output_tokens", h.outputTokens, "duration", time.Since(h.startTime))
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

func (h *streamHandler) popActiveBlockStopDataLocked() (string, bool) {
	if h.activeBlockType == "" {
		return "", false
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
		return "", false
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
	if err != nil {
		return "", false
	}
	return string(stopData), true
}

func (h *streamHandler) closeActiveBlockLocked() {
	stopData, ok := h.popActiveBlockStopDataLocked()
	if !ok {
		return
	}
	h.writeSSELocked("content_block_stop", stopData)
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

func (h *streamHandler) emitTextBlock(text string) {
	h.emitTextBlockWithWriter(text, h.writeSSE)
}

func (h *streamHandler) emitTextBlockWithWriter(text string, write func(event, data string)) {
	if !h.isStream || text == "" {
		return
	}
	h.markTextOutput()

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
	write("content_block_start", string(startData))

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
	write("content_block_delta", string(deltaData))

	stopMap := perf.AcquireMap()
	stopMap["type"] = "content_block_stop"
	stopMap["index"] = idx
	stopData, _ := json.Marshal(stopMap)
	perf.ReleaseMap(stopMap)
	write("content_block_stop", string(stopData))
}

func (h *streamHandler) markTextOutput() {
	h.mu.Lock()
	h.hasTextOutput = true
	h.mu.Unlock()
}

func (h *streamHandler) emitWriteChunkFallbackIfNeeded(write func(event, data string)) {
	if h.writeChunkBuffer == nil {
		return
	}

	h.mu.Lock()
	if h.hasTextOutput || h.writeChunkBuffer.Len() == 0 {
		h.mu.Unlock()
		return
	}
	text := h.writeChunkBuffer.String()
	h.hasTextOutput = true
	h.mu.Unlock()

	if h.isStream {
		h.emitTextBlockWithWriter(text, write)
		return
	}

	h.mu.Lock()
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type": "text",
		"text": text,
	})
	h.mu.Unlock()
}

func (h *streamHandler) handleToolCallAfterChecks(call toolCall) {
	h.pendingToolCalls = append(h.pendingToolCalls, call)
	h.toolCallCount++
}

func (h *streamHandler) shouldAcceptToolCall(call toolCall) bool {
	nameKey := strings.ToLower(strings.TrimSpace(call.name))
	if nameKey == "" {
		return false
	}
	if !hasRequiredToolInput(call.name, call.input) {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("invalid tool call suppressed", "tool", call.name, "input", call.input)
		}
		return false
	}
	if key := sideEffectToolDedupKey(nameKey, call.input); key != "" {
		maskedKey := maskDedupKey(key)
		h.mu.Lock()
		if _, ok := h.bashCallDedup[key]; ok {
			h.toolDedupCount++
			h.toolDedupKeys[maskedKey]++
			suppressed := h.toolDedupCount
			h.mu.Unlock()
			if h.config != nil && h.config.DebugEnabled {
				slog.Debug("duplicate mutating tool call suppressed", "tool", call.name, "dedup_key", maskedKey, "suppressed_total", suppressed)
			}
			return false
		}
		h.bashCallDedup[key] = struct{}{}
		h.seedToolDedup[key] = struct{}{}
		h.mu.Unlock()
	}
	return true
}

func maskDedupKey(key string) string {
	tool := key
	if idx := strings.IndexByte(tool, ':'); idx > 0 {
		tool = tool[:idx]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%s#%x", tool, h.Sum64())
}

func sideEffectToolDedupKey(nameKey, input string) string {
	switch nameKey {
	case "bash", "write", "edit":
	default:
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return ""
	}
	switch nameKey {
	case "bash":
		command, _ := payload["command"].(string)
		if strings.TrimSpace(command) == "" {
			command, _ = payload["cmd"].(string)
		}
		command = strings.TrimSpace(command)
		if command == "" {
			return ""
		}
		return "bash:" + command
	case "write":
		path := extractPathFromInput(payload)
		if path == "" {
			return ""
		}
		content, ok := payload["content"]
		if !ok {
			return ""
		}
		return "write:" + path + "\x00" + canonicalToolValue(content)
	case "edit":
		path := extractPathFromInput(payload)
		if path == "" {
			return ""
		}
		oldV, hasOld := payload["old_string"]
		newV, hasNew := payload["new_string"]
		if !hasOld || !hasNew {
			return ""
		}
		return "edit:" + path + "\x00" + canonicalToolValue(oldV) + "\x00" + canonicalToolValue(newV)
	default:
		return ""
	}
}

func extractPathFromInput(payload map[string]interface{}) string {
	if v, ok := payload["file_path"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := payload["path"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func canonicalToolValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(raw)
	}
}

func fallbackToolCallID(toolName, input string) string {
	nameKey := strings.ToLower(strings.TrimSpace(toolName))
	if nameKey == "" {
		return ""
	}
	normalizedInput := strings.TrimSpace(input)
	if normalizedInput == "" {
		normalizedInput = "{}"
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(nameKey))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(normalizedInput))
	return fmt.Sprintf("tool_anon_%x", h.Sum64())
}

func hasRequiredToolInput(name, input string) bool {
	nameKey := strings.ToLower(strings.TrimSpace(name))
	if nameKey == "" {
		return false
	}
	if input == "" {
		input = "{}"
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		// For known structured tools, malformed JSON should be treated as invalid.
		switch nameKey {
		case "edit", "write", "bash", "read", "glob", "grep":
			return false
		default:
			return true
		}
	}

	requireString := func(key string) bool {
		v, ok := payload[key].(string)
		return ok && strings.TrimSpace(v) != ""
	}

	switch nameKey {
	case "edit":
		_, hasOld := payload["old_string"]
		_, hasNew := payload["new_string"]
		hasPath := requireString("file_path") || requireString("path")
		return hasPath && hasOld && hasNew
	case "write":
		// Warp sometimes sends "path" instead of "file_path", or we might have mapped it.
		// Also strict checking might fail if "content" is empty string (though rare for meaningful write).
		_, hasContent := payload["content"]
		hasPath := requireString("file_path") || requireString("path")
		return hasPath && hasContent
	case "bash":
		return requireString("command") || requireString("cmd")
	case "read":
		return requireString("file_path") || requireString("path")
	default:
		return true
	}
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
		len(h.toolCallEmitted) > 0
	hasOutput := h.outputBuilder.Len() > 0 || h.responseText.Len() > 0 || len(h.contentBlocks) > 0
	h.mu.Unlock()

	// 上游无任何有效输出时，注入空响应提示避免客户端收到完全空的回复
	if !hasToolCalls && !hasOutput {
		slog.Warn("上游未返回有效内容，注入空响应提示")
		h.ensureBlock("text")
		h.mu.Lock()
		internalIdx := h.activeTextBlockIndex
		sseIdx := h.activeTextSSEIndex
		h.mu.Unlock()

		emptyMsg := "No response from upstream. The request may not be supported in this mode."
		if h.isStream {
			deltaMap := map[string]interface{}{
				"type":  "content_block_delta",
				"index": sseIdx,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": emptyMsg,
				},
			}
			deltaData, _ := json.Marshal(deltaMap)
			h.writeSSE("content_block_delta", string(deltaData))
		} else {
			h.responseText.WriteString(emptyMsg)
			if builder, ok := h.textBlockBuilders[internalIdx]; ok {
				builder.WriteString(emptyMsg)
			}
		}
	}

	stopReason := "end_turn"
	if hasToolCalls {
		stopReason = "tool_use"
	}
	slog.Warn("上游未发送结束标记，强制结束响应", "stop_reason", stopReason)
	h.finishResponse(stopReason)
}

func (h *streamHandler) shouldSkipIntroDelta(delta string) bool {
	key := normalizeIntroKey(delta)
	if key == "" {
		return false
	}
	h.mu.Lock()
	_, exists := h.introDedup[key]
	if !exists {
		h.introDedup[key] = struct{}{}
	}
	h.mu.Unlock()
	return exists
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

func extractEventMessage(event map[string]interface{}, fallback string) string {
	if event == nil {
		return fallback
	}
	if data, ok := event["data"].(map[string]interface{}); ok {
		if msg, ok := data["message"].(string); ok && strings.TrimSpace(msg) != "" {
			return strings.TrimSpace(msg)
		}
	}
	if msg, ok := event["message"].(string); ok && strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	return fallback
}

func (h *streamHandler) handleMessage(msg upstream.SSEMessage) {
	if h.config.DebugEnabled && msg.Type != "content_block_delta" {
		slog.Debug("Incoming SSE", "type", msg.Type)
	}
	h.mu.Lock()
	done := h.hasReturn
	h.mu.Unlock()
	if done {
		return
	}

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
	case "model.conversation_id":
		if msg.Event != nil {
			if id, ok := msg.Event["id"].(string); ok && id != "" && h.onConversationID != nil {
				h.onConversationID(id)
			}
		}

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
		h.markTextOutput()

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

	case "coding_agent.credits_exhausted":
		errorMsg := extractEventMessage(msg.Event, "You have run out of credits. Please upgrade your plan to continue.")
		h.closeActiveBlock()
		h.InjectErrorText("Injecting credits exhausted message to client", errorMsg)
		h.finishResponse("end_turn")
		return

	case "coding_agent.Write.started", "coding_agent.Edit.edit.started":
		if h.isStream {
			data, _ := msg.Event["data"].(map[string]interface{})
			path, _ := data["file_path"].(string)
			if !h.suppressThinking {
				op := "Writing"
				if strings.Contains(msg.Type, "Edit") {
					op = "Editing"
				}
				h.ensureBlock("thinking")
				h.emitThinkingDelta(fmt.Sprintf("\n[%s %s...]\n", op, path))

				rawData, _ := json.Marshal(msg.Event)
				h.writeSSE(msg.Type, string(rawData))
			}
		}
		return

	case "coding_agent.Write.content.chunk", "coding_agent.Edit.edit.chunk":
		if h.isStream {
			data, _ := msg.Event["data"].(map[string]interface{})
			text, _ := data["text"].(string)
			if text != "" {
				h.mu.Lock()
				if h.writeChunkBuffer != nil {
					h.writeChunkBuffer.WriteString(text)
				}
				h.mu.Unlock()
				// In no-thinking mode, surface Orchids write chunks as normal text deltas
				// so clients still see visible output instead of only internal events.
				if h.suppressThinking {
					h.emitTextDelta(text)
				} else {
					// Map Orchids code chunks to thinking blocks for standard UIs.
					h.emitThinkingDelta(text)
				}
			}
			if !h.suppressThinking {
				rawData, _ := json.Marshal(msg.Event)
				h.writeSSE(msg.Type, string(rawData))
			}
		}
		return

	case "coding_agent.Write.content.completed", "coding_agent.Edit.edit.completed", "coding_agent.edit_file.completed":
		if h.isStream {
			if !h.suppressThinking {
				h.emitThinkingDelta("\n[Done]\n")
				data, _ := json.Marshal(msg.Event)
				h.writeSSE(msg.Type, string(data))
			}
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
		// Just pass through the event, no internal tool result handling in proxy mode
		return

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
			if buf, ok := h.toolInputBuffers[toolID]; ok {
				perf.ReleaseStringBuilder(buf)
			}
			delete(h.toolInputBuffers, toolID)
			delete(h.toolInputHadDelta, toolID)
			delete(h.toolInputNames, toolID)
			return
		}
		inputStr := ""
		if buf, ok := h.toolInputBuffers[toolID]; ok {
			inputStr = strings.TrimSpace(buf.String())
			perf.ReleaseStringBuilder(buf)
		}
		inputStr = sanitizeToolInput(name, inputStr)
		delete(h.toolInputBuffers, toolID)
		delete(h.toolInputHadDelta, toolID)
		delete(h.toolInputNames, toolID)
		if h.toolCallHandled[toolID] {
			return
		}
		call := toolCall{id: toolID, name: name, input: inputStr}
		if !h.shouldAcceptToolCall(call) {
			return
		}
		h.toolCallHandled[toolID] = true
		if h.isStream {
			if inputStr != "" {
				h.addOutputTokens(inputStr)
			}
			h.emitToolUseFromInput(toolID, name, inputStr)
			return
		}
		h.handleToolCallAfterChecks(call)

	case "model.tool-call":
		toolID, _ := msg.Event["toolCallId"].(string)
		toolName, _ := msg.Event["toolName"].(string)
		inputStr, _ := msg.Event["input"].(string)
		inputStr = sanitizeToolInput(toolName, inputStr)
		if toolID == "" {
			toolID = fallbackToolCallID(toolName, inputStr)
			if toolID == "" {
				return
			}
		}
		if h.toolCallHandled[toolID] {
			return
		}
		call := toolCall{id: toolID, name: toolName, input: inputStr}
		if !h.shouldAcceptToolCall(call) {
			return
		}
		if h.currentToolInputID == toolID {
			h.currentToolInputID = ""
		}
		if buf, ok := h.toolInputBuffers[toolID]; ok {
			perf.ReleaseStringBuilder(buf)
		}
		delete(h.toolInputBuffers, toolID)
		delete(h.toolInputHadDelta, toolID)
		delete(h.toolInputNames, toolID)
		h.toolCallHandled[toolID] = true
		if h.isStream {
			h.emitToolUseFromInput(toolID, toolName, inputStr)
			return
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
		hadToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
			toolUseEmitted
		h.mu.Unlock()

		// Force stopReason to tool_use if we have emitted tool calls
		if toolUseEmitted {
			stopReason = "tool_use"
		}

		// If upstream claims tool_use but we didn't actually handle any tool calls, treat as end_turn.
		if stopReason == "tool_use" && !hadToolCalls {
			stopReason = "end_turn"
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

func (h *streamHandler) emitTextDelta(delta string) {
	if delta == "" {
		return
	}
	h.markTextOutput()

	h.mu.Lock()
	sseIdx := h.activeTextSSEIndex
	internalIdx := h.activeTextBlockIndex
	h.mu.Unlock()

	if sseIdx < 0 {
		sseIdx = h.ensureBlock("text")
		h.mu.Lock()
		internalIdx = h.activeTextBlockIndex
		h.mu.Unlock()
	}

	h.addOutputTokens(delta)

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
}

// InjectErrorText injects an error message as a text delta into the stream or buffer.
func (h *streamHandler) InjectErrorText(logMsg, errorMsg string) {
	if h.config != nil && h.config.DebugEnabled {
		slog.Info(logMsg, "error_msg", errorMsg, "is_stream", h.isStream)
	}
	h.markTextOutput()
	idx := h.ensureBlock("text")
	internalIdx := h.activeTextBlockIndex

	if h.isStream {
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = idx

		delta := perf.AcquireMap()
		delta["type"] = "text_delta"
		delta["text"] = errorMsg
		m["delta"] = delta

		data, _ := json.Marshal(m)
		h.writeSSE("content_block_delta", string(data))

		perf.ReleaseMap(delta)
		perf.ReleaseMap(m)
	} else {
		h.mu.Lock()
		if builder, ok := h.textBlockBuilders[internalIdx]; ok {
			builder.WriteString(errorMsg)
		}
		h.mu.Unlock()
	}
}

func (h *streamHandler) InjectAuthError(category, errStr string) {
	var errorMsg string
	switch {
	case strings.Contains(errStr, "401"):
		errorMsg = "Authentication Error: Session expired (401). Please update your account credentials."
	case strings.Contains(errStr, "403"):
		errorMsg = "Access Forbidden (403): Your account might be flagged or blocked. Try re-enabling it in the Admin UI."
	default:
		errorMsg = fmt.Sprintf("Request Failed: %s. Please check your account status.", errStr)
	}
	h.InjectErrorText("Injecting auth error to client", errorMsg)
}

func (h *streamHandler) InjectRetryExhaustedError(lastErr string) {
	errorMsg := fmt.Sprintf("Request failed: retries exhausted. Last error: %s", lastErr)
	h.InjectErrorText("Injecting retry exhausted error to client", errorMsg)
}

func (h *streamHandler) InjectNoAvailableAccountError(lastErr string, selectErr error) {
	errorMsg := "Request failed: retries exhausted and no available accounts. Please check account statuses in Admin UI or add valid accounts."
	if selectErr != nil {
		errorMsg = fmt.Sprintf("%s (selector: %v, last error: %s)", errorMsg, selectErr, lastErr)
	}
	h.InjectErrorText("Injecting no available account error to client", errorMsg)
}
