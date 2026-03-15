package handler

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/orchids"
	"orchids-api/internal/perf"
	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
)

var orchidsToolMarkerRegex = regexp.MustCompile(`Used tool: (\w+)\s+with input: (\{.*\})`)

const (
	fnv64Offset = uint64(14695981039346656037)
	fnv64Prime  = uint64(1099511628211)
)

const (
	sseEventPrefix                 = "event: "
	sseDataPrefix                  = "data: "
	sseLineBreak                   = "\n\n"
	sseDataJoin                    = "\ndata: "
	sseDoneLine                    = "data: [DONE]\n\n"
	sseKeepAlive                   = ": keep-alive\n\n"
	sseDeferredFlushFrameThreshold = 4
	sseDeferredFlushByteThreshold  = 2048
	sseBufferedWriteMax            = 4096
)

var (
	rawJSONEmptyObject  = json.RawMessage("{}")
	sseTextDeltaMarker  = []byte(`"type":"text_delta"`)
	sseDoneLineBytes    = []byte(sseDoneLine)
	sseKeepAliveBytes   = []byte(sseKeepAlive)
	sseEventPrefixBytes = []byte(sseEventPrefix)
	sseDataPrefixBytes  = []byte(sseDataPrefix)
	sseLineBreakBytes   = []byte(sseLineBreak)
	sseDataJoinBytes    = []byte(sseDataJoin)
	sseEventBytesByName = map[string][]byte{
		"message_start":       []byte("message_start"),
		"message_delta":       []byte("message_delta"),
		"message_stop":        []byte("message_stop"),
		"content_block_start": []byte("content_block_start"),
		"content_block_delta": []byte("content_block_delta"),
		"content_block_stop":  []byte("content_block_stop"),
		"fs_operation":        []byte("fs_operation"),
	}
	quotedPathRegex = regexp.MustCompile(`"([^"\n\r]+)"`)
)

func mapKeys(m map[string]interface{}) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Order isn't critical; keep lightweight (avoid importing sort).
	return keys
}

// --- sse_frame structs removed ---

type sseMessageStop struct {
	Type string `json:"type"`
}

type directToolUseState struct {
	id    string
	name  string
	input *strings.Builder
}

func marshalJSONString(v interface{}) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalEventPayloadBytes(msg upstream.SSEMessage) ([]byte, error) {
	if len(msg.RawJSON) > 0 {
		return msg.RawJSON, nil
	}
	return json.Marshal(msg.Event)
}

func marshalEventPayload(msg upstream.SSEMessage) (string, error) {
	raw, err := marshalEventPayloadBytes(msg)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func writeSSEFrame(w io.Writer, event, data string) error {
	if _, err := io.WriteString(w, sseEventPrefix); err != nil {
		return err
	}
	if _, err := io.WriteString(w, event); err != nil {
		return err
	}
	if _, err := io.WriteString(w, sseDataJoin); err != nil {
		return err
	}
	if _, err := io.WriteString(w, data); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseLineBreak)
	return err
}

func writeOpenAIFrame(w io.Writer, payload []byte) error {
	if _, err := w.Write(sseDataPrefixBytes); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write(sseLineBreakBytes)
	return err
}

func writeSSEEventName(w io.Writer, event string) error {
	if raw, ok := sseEventBytesByName[event]; ok {
		_, err := w.Write(raw)
		return err
	}
	if sw, ok := w.(io.StringWriter); ok {
		_, err := sw.WriteString(event)
		return err
	}
	_, err := w.Write([]byte(event))
	return err
}

func writeSSEFrameBytes(w io.Writer, event string, data []byte) error {
	if _, err := w.Write(sseEventPrefixBytes); err != nil {
		return err
	}
	if err := writeSSEEventName(w, event); err != nil {
		return err
	}
	if _, err := w.Write(sseDataJoinBytes); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.Write(sseLineBreakBytes)
	return err
}

func shouldFlushSSEImmediately(event, data string) bool {
	switch event {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_stop":
		return true
	case "content_block_delta":
		return strings.Contains(data, `"type":"text_delta"`)
	case "fs_operation":
		return false
	}
	return !strings.HasPrefix(event, "coding_agent.")
}

func (h *streamHandler) flushSSEWithLenLocked(event string, dataLen int, immediate bool, force bool) {
	if h.flusher == nil {
		return
	}
	if force || immediate {
		h.deferredFlushFrames = 0
		h.deferredFlushBytes = 0
		h.flusher.Flush()
		return
	}
	h.deferredFlushFrames++
	h.deferredFlushBytes += len(event) + dataLen + len(sseEventPrefix) + len(sseDataJoin) + len(sseLineBreak)
	if h.deferredFlushFrames >= sseDeferredFlushFrameThreshold || h.deferredFlushBytes >= sseDeferredFlushByteThreshold {
		h.deferredFlushFrames = 0
		h.deferredFlushBytes = 0
		h.flusher.Flush()
	}
}

func (h *streamHandler) flushSSELocked(event, data string, force bool) {
	h.flushSSEWithLenLocked(event, len(data), shouldFlushSSEImmediately(event, data), force)
}

func shouldFlushSSEImmediatelyBytes(event string, data []byte) bool {
	switch event {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_stop":
		return true
	case "content_block_delta":
		return bytes.Contains(data, sseTextDeltaMarker)
	case "fs_operation":
		return false
	}
	return !strings.HasPrefix(event, "coding_agent.")
}

func (h *streamHandler) flushSSEBytesLocked(event string, data []byte, force bool) {
	h.flushSSEWithLenLocked(event, len(data), shouldFlushSSEImmediatelyBytes(event, data), force)
}

func (h *streamHandler) flushSSEBytesLockedWithHint(event string, dataLen int, immediate bool, force bool) {
	h.flushSSEWithLenLocked(event, dataLen, immediate, force)
}

// --- json helper functions removed ---

// --- sse framing functions removed ---

type streamHandler struct {
	// Configuration
	config            *config.Config
	workdir           string
	isStream          bool
	suppressThinking  bool
	useUpstreamUsage  bool
	outputTokenMode   string
	responseFormat    adapter.ResponseFormat
	disallowToolCalls bool
	allowedToolNames  map[string]struct{}

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
	completionLogged         bool
	finalStopReason          string
	outputTokens             int
	thinkingTokens           int
	inputTokens              int
	activeThinkingBlockIndex int
	activeThinkingSSEIndex   int
	activeTextBlockIndex     int
	activeTextSSEIndex       int
	activeBlockType          string // "thinking", "text", "tool_use"

	// Buffers and Builders
	responseText          *strings.Builder
	outputEstimator       tiktoken.Estimator
	writeChunkBuffer      *strings.Builder
	textBlockBuilders     map[int]*strings.Builder
	thinkingBlockBuilders map[int]*strings.Builder
	thinkingBlockSigs     map[int]string
	contentBlocks         []map[string]interface{}
	currentTextIndex      int
	pendingThinkingSig    string
	hasTextOutput         bool
	lastTextDelta         string
	lastTextDeltaSource   string
	lastTextDeltaAt       time.Time
	deferredFlushFrames   int
	deferredFlushBytes    int
	openAIChunkScratch    []byte
	ssePayloadScratch     []byte

	// Tool Handling (proxy mode only)
	toolBlocks                  map[string]int
	pendingToolCalls            []toolCall
	toolInputNames              map[string]string
	toolInputBuffers            map[string]*strings.Builder
	toolInputHadDelta           map[string]bool
	pendingDirectToolUses       map[int]*directToolUseState
	toolCallHandled             map[string]bool
	toolCallEmitted             map[string]struct{}
	currentToolInputID          string
	toolCallCount               int
	skippedDirectBlockIndices   map[int]struct{}
	suppressedToolCalls         int
	bashCallDedup               map[string]struct{}
	seedToolDedup               map[string]struct{}
	toolDedupCount              int
	toolDedupKeys               map[string]int
	introDedup                  map[string]struct{}
	noToolsFallbackText         string
	suppressEmptyOutputFallback bool

	// Throttling
	lastScanTime time.Time

	// Callbacks
	onConversationID func(string) // 濠电姷鏁搁崑鐐哄垂閸洖绠伴柟闂寸劍閺呮繈鏌曟径鍡樻珕闁稿顦甸弻銈囩矙鐠恒劋绮垫繛瀛樺殠閸婃繈寮婚敓鐘茬＜婵炴垶锕╅崵瀣磽娴ｆ彃浜鹃梺?conversationID 闂傚倸鍊风粈渚€骞栭锕€鐤柛鎰ゴ閺嬫牗绻涢幋鐐╂（婵炲樊浜滈崘鈧銈嗗姧缁蹭粙顢?
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

		blockIndex:                -1,
		toolBlocks:                make(map[string]int),
		responseText:              perf.AcquireStringBuilder(),
		writeChunkBuffer:          perf.AcquireStringBuilder(),
		textBlockBuilders:         make(map[int]*strings.Builder),
		thinkingBlockBuilders:     make(map[int]*strings.Builder),
		thinkingBlockSigs:         make(map[int]string),
		toolInputNames:            make(map[string]string),
		toolInputBuffers:          make(map[string]*strings.Builder),
		toolInputHadDelta:         make(map[string]bool),
		pendingDirectToolUses:     make(map[int]*directToolUseState),
		toolCallHandled:           make(map[string]bool),
		toolCallEmitted:           make(map[string]struct{}),
		skippedDirectBlockIndices: make(map[int]struct{}),
		bashCallDedup:             make(map[string]struct{}),
		seedToolDedup:             make(map[string]struct{}),
		toolDedupKeys:             make(map[string]int),
		introDedup:                make(map[string]struct{}),
		allowedToolNames:          make(map[string]struct{}),
		msgID:                     fmt.Sprintf("msg_%d", time.Now().UnixMilli()),
		startTime:                 time.Now(),
		currentTextIndex:          -1,
		activeThinkingBlockIndex:  -1,
		activeThinkingSSEIndex:    -1,
		activeTextBlockIndex:      -1,
		activeTextSSEIndex:        -1,
		activeBlockType:           "",
		openAIChunkScratch:        make([]byte, 0, 512),
		ssePayloadScratch:         make([]byte, 0, 512),
	}
	return h
}

func (h *streamHandler) setNoToolsFallbackText(text string) {
	h.mu.Lock()
	h.noToolsFallbackText = strings.TrimSpace(text)
	h.mu.Unlock()
}

func (h *streamHandler) setSuppressEmptyOutputFallback(suppress bool) {
	h.mu.Lock()
	h.suppressEmptyOutputFallback = suppress
	h.mu.Unlock()
}

func (h *streamHandler) setDisallowToolCalls(disallow bool) {
	h.mu.Lock()
	h.disallowToolCalls = disallow
	h.mu.Unlock()
}

func (h *streamHandler) setAllowedToolNames(names []string) {
	h.mu.Lock()
	clear(h.allowedToolNames)
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		h.allowedToolNames[key] = struct{}{}
	}
	h.mu.Unlock()
}

func (h *streamHandler) release() {
	perf.ReleaseStringBuilder(h.responseText)
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
	for _, item := range h.pendingDirectToolUses {
		if item != nil && item.input != nil {
			perf.ReleaseStringBuilder(item.input)
		}
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
		written, err := h.writeOpenAISSE(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSELocked(event, data, false)
		}
		return
	}

	if err := writeSSEFrame(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSELocked(event, data, false)

	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, data)
	}
}

func (h *streamHandler) writeSSEBytes(event string, data []byte) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasReturn {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSEBytesLocked(event, data, false)
		}
		return
	}

	if err := writeSSEFrameBytes(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSEBytesLocked(event, data, false)
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, string(data))
	}
}

func (h *streamHandler) writeOpenAISSE(event, data string) (bool, error) {
	return h.writeOpenAISSEBytes(event, []byte(data))
}
func (h *streamHandler) writeOpenAISSEBytes(event string, data []byte) (bool, error) {
	raw, ok := adapter.AppendOpenAIChunk(h.openAIChunkScratch[:0], h.msgID, h.startTime.Unix(), event, data)
	if !ok {
		return false, nil
	}
	h.openAIChunkScratch = raw[:0]
	if err := writeOpenAIFrame(h.w, raw); err != nil {
		return false, err
	}
	return true, nil
}

func (h *streamHandler) writeFinalSSE(event, data string) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSE(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSELocked(event, data, true)
		}
		// Send [DONE] at the very end
		if event == "message_stop" {
			if _, err := h.w.Write(sseDoneLineBytes); err != nil {
				h.markWriteErrorLocked(event, err)
				return
			}
			h.flushSSELocked(event, sseDoneLine, true)
		}
		return
	}

	if err := writeSSEFrame(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSELocked(event, data, true)

	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, data)
	}
}

func (h *streamHandler) writeFinalSSEBytes(event string, data []byte) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeFinalSSEBytesLocked(event, data)
}

func (h *streamHandler) writeFinalSSEBytesLocked(event string, data []byte) {
	h.writeFinalSSEBytesLockedWithHint(event, data, false)
}

func (h *streamHandler) writeFinalSSEBytesLockedWithHint(event string, data []byte, immediate bool) {
	if !h.isStream {
		return
	}

	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSEBytesLockedWithHint(event, len(data), immediate, true)
		}
		if event == "message_stop" {
			if _, err := h.w.Write(sseDoneLineBytes); err != nil {
				h.markWriteErrorLocked(event, err)
				return
			}
			h.flushSSELocked(event, sseDoneLine, true)
		}
		return
	}

	if err := writeSSEFrameBytes(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSEBytesLockedWithHint(event, len(data), immediate, true)
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, string(data))
	}
}

func (h *streamHandler) writeSSEBytesLockedWithHint(event string, data []byte, immediate bool) {
	if !h.isStream {
		return
	}
	if h.hasReturn {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSEBytesLockedWithHint(event, len(data), immediate, false)
		}
		return
	}
	if err := writeSSEFrameBytes(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSEBytesLockedWithHint(event, len(data), immediate, false)
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, string(data))
	}
	if h.config != nil && h.config.DebugEnabled {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}

func (h *streamHandler) writeSSEContentBlockStartToolUseLocked(index int, id, name string, final bool) {
	raw, err := appendSSEContentBlockStartToolUse(h.ssePayloadScratch[:0], index, id, name)
	if err != nil {
		h.markWriteErrorLocked("content_block_start", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_start", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
}

func (h *streamHandler) writeSSEContentBlockStartToolUse(index int, id, name string, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockStartToolUseLocked(index, id, name, final)
}

func (h *streamHandler) writeSSEContentBlockStartTextLocked(index int, final bool) {
	raw, err := appendSSEContentBlockStartText(h.ssePayloadScratch[:0], index)
	if err != nil {
		h.markWriteErrorLocked("content_block_start", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_start", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
}

func (h *streamHandler) writeSSEContentBlockStartText(index int, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockStartTextLocked(index, final)
}

func (h *streamHandler) writeSSEContentBlockDeltaInputJSONLocked(index int, partialJSON string, final bool) {
	raw, err := appendSSEContentBlockDeltaInputJSON(h.ssePayloadScratch[:0], index, partialJSON)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, false)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, false)
}

func (h *streamHandler) writeSSEContentBlockDeltaInputJSON(index int, partialJSON string, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockDeltaInputJSONLocked(index, partialJSON, final)
}

func (h *streamHandler) writeSSEContentBlockDeltaTextLocked(index int, text string, final bool) {
	raw, err := appendSSEContentBlockDeltaText(h.ssePayloadScratch[:0], index, text)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, true)
}

func (h *streamHandler) writeSSEContentBlockDeltaText(index int, text string, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockDeltaTextLocked(index, text, final)
}

func (h *streamHandler) writeSSEContentBlockDeltaThinkingLocked(index int, thinking string, final bool) {
	raw, err := appendSSEContentBlockDeltaThinking(h.ssePayloadScratch[:0], index, thinking)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, false)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, false)
}

func (h *streamHandler) writeSSEContentBlockDeltaThinking(index int, thinking string, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockDeltaThinkingLocked(index, thinking, final)
}

func (h *streamHandler) writeSSEContentBlockStopLocked(index int, final bool) {
	raw, err := appendSSEContentBlockStop(h.ssePayloadScratch[:0], index)
	if err != nil {
		h.markWriteErrorLocked("content_block_stop", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_stop", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_stop", raw, true)
}

func (h *streamHandler) writeSSEContentBlockStop(index int, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEContentBlockStopLocked(index, final)
}

func (h *streamHandler) writeSSEMessageDeltaLocked(stopReason string, outputTokens int, final bool) {
	raw, err := appendSSEMessageDelta(h.ssePayloadScratch[:0], stopReason, outputTokens)
	if err != nil {
		h.markWriteErrorLocked("message_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("message_delta", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("message_delta", raw, true)
}

func (h *streamHandler) writeSSEMessageDelta(stopReason string, outputTokens int, final bool) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writeSSEMessageDeltaLocked(stopReason, outputTokens, final)
}

func (h *streamHandler) writeSSEMessageStart(model string, inputTokens, outputTokens int) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := appendSSEMessageStart(h.ssePayloadScratch[:0], h.msgID, model, inputTokens, outputTokens)
	if err != nil {
		h.markWriteErrorLocked("message_start", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	h.writeSSEBytesLockedWithHint("message_start", raw, true)
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
	if _, err := h.w.Write(sseKeepAliveBytes); err != nil {
		h.markWriteErrorLocked("keep-alive", err)
		return
	}
	h.flushSSELocked("keep-alive", sseKeepAlive, true)
}

func (h *streamHandler) addThinkingTokens(text string) {
	if text == "" {
		return
	}
	h.outputMu.Lock()
	if !h.useUpstreamUsage {
		// we skip the estimator for thinking to keep it for billing/output only
		// or we can add to thinkingTokens directly if we use an estimator
		h.thinkingTokens += tiktoken.EstimateTextTokens(text)
		h.outputTokens += tiktoken.EstimateTextTokens(text)
	}
	h.outputMu.Unlock()
}

func (h *streamHandler) addOutputTokens(text string) {
	if text == "" {
		return
	}
	h.outputMu.Lock()
	if !h.useUpstreamUsage {
		h.outputEstimator.Add(text)
	}
	h.outputMu.Unlock()
}

func (h *streamHandler) finalizeOutputTokens() {
	h.outputMu.Lock()
	defer h.outputMu.Unlock()

	if h.useUpstreamUsage {
		return
	}
	h.outputTokens = h.outputEstimator.Count()
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

	// 濠电姷鏁搁崑鐐哄垂閸洖绠伴柛婵勫劤閻捇鏌ｉ幋婵愭綗闁逞屽墮閹虫﹢寮崘顔肩＜婵﹩鍘肩粊鍫曟⒒娓氣偓濞佳団€﹂崼銉ョ閹艰揪绲挎稉宥囨喐閻楀牆绗氶柍閿嬪灴閺岀喓绱掑Ο铏诡儌闂佺粯甯楅幃鍌炲蓟濞戙垺鏅查柛娑卞枟閸犳劗绱?h.blockIndex闂?	// 濠电姷鏁搁崕鎴犲緤閽樺娲晜閻愵剙搴婇梺鍛婃处閸ㄦ澘效閺屻儲鐓冪憸婊堝礈濞戞碍顫曢柟鐑樻尵閻熷綊鏌涢…鎴濇灓濞寸姾鍋愮槐鎾存媴閻熼偊鏆㈤梺鍝勬噽婵炩偓鐎殿喖顭峰畷銊╁级閹寸媭鍞洪梻浣筋潐閹矂宕㈤挊澶樼唵闁哄啫鐗婇埛鎴︽煕濞戞﹫鍔熼柟铏姍閺屾盯濡搁妸銉у帿闁诲酣娼ч妶鎼佸春閿熺姴宸濇い鎾跺濡差垶鏌ｆ惔锛勭暛闁稿酣浜惰棟妞ゆ牗鍩冮弸宥夋煏韫囧鈧牠宕戦敐澶嬬厱闁靛绲芥俊鐣岀磼閳ь剟宕橀埡鈧换鍡涙煟閹邦厼绲婚柍褜鍓濋褍宓勯梺鍦濠㈡﹢锝為崨瀛樼厽婵☆垰鍚嬮弳鈺呮煃鐟欏嫮娲存慨濠冩そ楠炴牠鎮欓幓鎺懶戦梻浣侯焾椤戝洭宕伴幘璇茬闁圭儤顨忛弫鍐煥閺冨洤袚婵炲懏鐗犻弻锝堢疀閺囩偘鎴烽梺鐑╁墲濡啫鐣烽悽绋课у璺侯儑閸橀箖姊绘担鍝ヤ虎妞ゆ垵妫涚槐鐐哄箣閻愵亙绨婚梺瑙勫劤绾绢厾绮旈悜姗嗘闁绘劕妯婇崕鎰亜閿旀儳顣奸柟顖涙椤㈡瑩鎳￠妶鍥风闯闂傚倸鍊烽懗鍫曘€佹繝鍕濞村吋娼欑壕鍧楁煟閵忋埄鐒鹃柡?"Mismatched content block type"闂?
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
	for _, item := range h.pendingDirectToolUses {
		if item != nil && item.input != nil {
			perf.ReleaseStringBuilder(item.input)
		}
	}
	clear(h.pendingDirectToolUses)

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
	h.thinkingTokens = 0
	h.completionLogged = false
	h.outputEstimator.Reset()
	h.writeChunkBuffer.Reset()
	h.useUpstreamUsage = false
	h.finalStopReason = ""
	h.hasTextOutput = false
	h.lastTextDelta = ""
	h.lastTextDeltaSource = ""
	h.lastTextDeltaAt = time.Time{}
	h.deferredFlushFrames = 0
	h.deferredFlushBytes = 0
}

func (h *streamHandler) shouldEmitToolCalls(stopReason string) bool {
	return true
}

// seedSideEffectDedupFromMessages pre-seeds dedup keys from prior assistant tool_use blocks.
func (h *streamHandler) seedSideEffectDedupFromMessages(messages []prompt.Message) {
	if len(messages) == 0 {
		return
	}
	lastUserTextIdx := -1
	for i, msg := range messages {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		if strings.TrimSpace(msg.ExtractText()) != "" {
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

func (h *streamHandler) writeUpstreamEventSSE(msg upstream.SSEMessage) {
	if !h.isStream {
		return
	}
	payload, err := marshalEventPayloadBytes(msg)
	if err != nil {
		return
	}
	h.writeSSEBytes(msg.Type, payload)
}

func directSSEImmediate(event string, payload []byte) bool {
	if event != "content_block_delta" {
		return true
	}
	return bytes.Contains(payload, sseTextDeltaMarker)
}

func (h *streamHandler) WriteDirectSSE(event string, payload []byte, final bool) {
	if !h.isStream || len(payload) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !final && h.hasReturn {
		return
	}
	immediate := directSSEImmediate(event, payload)
	if final {
		h.writeFinalSSEBytesLockedWithHint(event, payload, immediate)
		return
	}
	h.writeSSEBytesLockedWithHint(event, payload, immediate)
}

func (h *streamHandler) ObserveTextDelta(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	h.markTextOutput()
	h.addOutputTokens(text)
}

func (h *streamHandler) ObserveThinkingDelta(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	h.addThinkingTokens(text)
}

func (h *streamHandler) ObserveToolCall(name, input string) {
	h.addOutputTokens(name)
	h.addOutputTokens(input)
	h.mu.Lock()
	h.toolCallCount++
	h.mu.Unlock()
}

func (h *streamHandler) ObserveUsage(inputTokens, outputTokens int) {
	h.setUsageTokens(inputTokens, outputTokens)
}

func (h *streamHandler) ObserveStopReason(stopReason string) {
	stopReason = strings.TrimSpace(stopReason)
	if stopReason == "" {
		return
	}
	h.mu.Lock()
	h.finalStopReason = stopReason
	h.mu.Unlock()
}

func (h *streamHandler) FinishDirectSSE(stopReason string) {
	stopReason = strings.TrimSpace(stopReason)
	if stopReason == "" {
		stopReason = "end_turn"
	}

	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	h.hasReturn = true
	if strings.TrimSpace(h.finalStopReason) == "" {
		h.finalStopReason = stopReason
	}
	stopReason = h.finalStopReason
	h.mu.Unlock()

	h.finalizeOutputTokens()
	h.finalizeCompletion(stopReason)
}

func directSSEEventType(event map[string]interface{}) string {
	if event == nil {
		return ""
	}
	if value, ok := event["type"].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func directSSEIndex(event map[string]interface{}) int {
	if event == nil {
		return -1
	}
	switch value := event["index"].(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return int(n)
		}
	}
	return -1
}

func (h *streamHandler) handleDirectFinalSSEEvent(msg upstream.SSEMessage) bool {
	switch msg.Type {
	case "message_start":
		event := msg.Event
		if directSSEEventType(event) != "message_start" {
			return false
		}
		h.writeUpstreamEventSSE(msg)
		return true

	case "message_delta":
		event := msg.Event
		if directSSEEventType(event) != "message_delta" {
			return false
		}
		if delta, ok := event["delta"].(map[string]interface{}); ok {
			if stopReason, ok := delta["stop_reason"].(string); ok && strings.TrimSpace(stopReason) != "" {
				h.mu.Lock()
				h.finalStopReason = strings.TrimSpace(stopReason)
				h.mu.Unlock()
			}
		}
		if usage, ok := event["usage"].(map[string]interface{}); ok {
			if raw, ok := usage["output_tokens"]; ok {
				switch value := raw.(type) {
				case int:
					h.setUsageTokens(-1, value)
				case float64:
					h.setUsageTokens(-1, int(value))
				}
			}
		}
		h.writeUpstreamEventSSE(msg)
		return true

	case "message_stop":
		event := msg.Event
		if directSSEEventType(event) != "message_stop" {
			return false
		}
		h.writeUpstreamEventSSE(msg)
		stopReason := "end_turn"
		h.mu.Lock()
		if !h.hasReturn {
			h.hasReturn = true
			if strings.TrimSpace(h.finalStopReason) == "" {
				h.finalStopReason = "end_turn"
			}
			stopReason = h.finalStopReason
		} else if strings.TrimSpace(h.finalStopReason) != "" {
			stopReason = h.finalStopReason
		}
		h.mu.Unlock()
		h.finalizeOutputTokens()
		h.finalizeCompletion(stopReason)
		return true

	case "content_block_start":
		event := msg.Event
		if directSSEEventType(event) != "content_block_start" {
			return false
		}
		index := directSSEIndex(event)
		if index < 0 {
			return false
		}
		contentBlock, _ := event["content_block"].(map[string]interface{})
		blockType, _ := contentBlock["type"].(string)
		blockType = strings.TrimSpace(blockType)
		if blockType == "thinking" && h.suppressThinking {
			h.mu.Lock()
			h.suppressEmptyOutputFallback = true
			h.skippedDirectBlockIndices[index] = struct{}{}
			h.mu.Unlock()
			return true
		}
		if blockType == "tool_use" {
			toolID, _ := contentBlock["id"].(string)
			toolName, _ := contentBlock["name"].(string)
			toolID = strings.TrimSpace(toolID)
			toolName = strings.TrimSpace(toolName)
			if toolID == "" || toolName == "" {
				return true
			}

			h.mu.Lock()
			if index > h.blockIndex {
				h.blockIndex = index
			}
			state, exists := h.pendingDirectToolUses[index]
			if !exists || state == nil {
				state = &directToolUseState{input: perf.AcquireStringBuilder()}
				h.pendingDirectToolUses[index] = state
			}
			state.id = toolID
			state.name = toolName
			h.mu.Unlock()
			return true
		}

		h.mu.Lock()
		if index > h.blockIndex {
			h.blockIndex = index
		}
		switch blockType {
		case "thinking":
			h.activeThinkingSSEIndex = index
			h.activeBlockType = "thinking"
		case "text":
			h.activeTextSSEIndex = index
			h.activeBlockType = "text"
		}
		h.mu.Unlock()

		h.writeUpstreamEventSSE(msg)
		return true

	case "content_block_delta":
		event := msg.Event
		if directSSEEventType(event) != "content_block_delta" {
			return false
		}
		index := directSSEIndex(event)
		if index < 0 {
			return false
		}

		h.mu.Lock()
		_, skipped := h.skippedDirectBlockIndices[index]
		directToolUse := h.pendingDirectToolUses[index]
		h.mu.Unlock()
		if skipped {
			return true
		}
		if directToolUse != nil {
			delta, _ := event["delta"].(map[string]interface{})
			deltaType, _ := delta["type"].(string)
			if strings.TrimSpace(deltaType) != "input_json_delta" {
				return true
			}
			partialJSON, _ := delta["partial_json"].(string)
			if partialJSON == "" {
				return true
			}
			h.mu.Lock()
			if current := h.pendingDirectToolUses[index]; current != nil && current.input != nil {
				current.input.WriteString(partialJSON)
			}
			h.mu.Unlock()
			return true
		}

		delta, _ := event["delta"].(map[string]interface{})
		deltaType, _ := delta["type"].(string)
		switch strings.TrimSpace(deltaType) {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text != "" {
				h.markTextOutput()
				h.addOutputTokens(text)
			}
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			if text != "" && h.isStream {
				h.addThinkingTokens(text)
			}
		}

		h.writeUpstreamEventSSE(msg)
		return true

	case "content_block_stop":
		event := msg.Event
		if directSSEEventType(event) != "content_block_stop" {
			return false
		}
		index := directSSEIndex(event)
		if index < 0 {
			return false
		}

		h.mu.Lock()
		if _, skipped := h.skippedDirectBlockIndices[index]; skipped {
			delete(h.skippedDirectBlockIndices, index)
			h.mu.Unlock()
			return true
		}
		if pending := h.pendingDirectToolUses[index]; pending != nil {
			delete(h.pendingDirectToolUses, index)
			h.mu.Unlock()

			inputStr := ""
			if pending.input != nil {
				inputStr = strings.TrimSpace(pending.input.String())
				perf.ReleaseStringBuilder(pending.input)
			}
			toolID := strings.TrimSpace(pending.id)
			toolName, normalizedInput := normalizeUpstreamToolCall(pending.name, inputStr, h.workdir)
			if toolID == "" {
				toolID = fallbackToolCallID(toolName, normalizedInput)
			}
			if toolID == "" || toolName == "" {
				return true
			}
			if h.toolCallHandled[toolID] {
				return true
			}
			call := toolCall{id: toolID, name: toolName, input: normalizedInput}
			h.toolCallHandled[toolID] = true
			if h.isStream {
				if _, ok := h.toolCallEmitted[toolID]; ok {
					return true
				}
				h.toolCallEmitted[toolID] = struct{}{}
				h.toolCallCount++
				h.emitToolCallStream(call, index, false)
				return true
			}
			h.handleToolCallAfterChecks(call)
			return true
		}
		if h.activeTextSSEIndex == index {
			h.activeTextSSEIndex = -1
			if h.activeBlockType == "text" {
				h.activeBlockType = ""
			}
		}
		if h.activeThinkingSSEIndex == index {
			h.activeThinkingSSEIndex = -1
			if h.activeBlockType == "thinking" {
				h.activeBlockType = ""
			}
		}
		h.mu.Unlock()

		h.writeUpstreamEventSSE(msg)
		return true
	}

	return false
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

	nameKey := strings.ToLower(strings.TrimSpace(name))
	switch nameKey {
	case "write", "edit", "read", "bash", "glob":
	default:
		return input
	}

	switch nameKey {
	case "write":
		if !strings.Contains(trimmed, `"path"`) && !strings.Contains(trimmed, `"overwrite"`) {
			return input
		}
	case "edit", "read":
		if !strings.Contains(trimmed, `"path"`) {
			return input
		}
	case "bash":
		if !strings.Contains(trimmed, `"cmd"`) {
			return input
		}
	case "glob":
		if !strings.Contains(trimmed, `"path"`) || strings.Contains(trimmed, `"pattern"`) {
			return input
		}
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return input
	}

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
	case "glob":
		if _, ok := payload["pattern"]; !ok {
			if path, ok := payload["path"].(string); ok && strings.TrimSpace(path) != "" {
				payload["pattern"] = "*"
				changed = true
			}
		}
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

func normalizeUpstreamToolCall(name, input, workdir string) (string, string) {
	rawName := strings.TrimSpace(name)
	if rawName == "" {
		return rawName, input
	}
	if bashInput, ok := rewriteDirectoryListToolInput(rawName, input, workdir); ok {
		return "Bash", bashInput
	}
	normalizedName := normalizeUpstreamToolName(rawName)
	sanitized := sanitizeToolInput(normalizedName, input)
	sanitized = rewriteForeignBashReadCommandInput(normalizedName, sanitized, workdir)
	sanitized = rewriteForeignAbsoluteToolPathInput(normalizedName, sanitized, workdir)
	if bashInput, ok := rewriteAbsoluteReadToBashFallback(normalizedName, sanitized, workdir); ok {
		return "Bash", bashInput
	}
	return normalizedName, sanitized
}

func normalizeUpstreamToolName(name string) string {
	mapped := orchids.NormalizeToolNameFallback(name)
	if strings.TrimSpace(mapped) == "" {
		return name
	}
	return mapped
}

func rewriteDirectoryListToolInput(name, input, workdir string) (string, bool) {
	if !isDirectoryListToolName(name) {
		return "", false
	}
	path := extractDirectoryListPath(input)
	if isPlaceholderDirectoryListPath(path) && strings.TrimSpace(workdir) != "" {
		path = strings.TrimSpace(workdir)
	}
	if strings.TrimSpace(path) == "" {
		path = strings.TrimSpace(workdir)
	}
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	payload := map[string]string{
		"command":     "ls -1A -- " + strconv.Quote(path),
		"description": "List top-level directory entries",
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	return string(normalized), true
}

func isDirectoryListToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ls", "listdir", "list_dir", "list_directory":
		return true
	default:
		return false
	}
}

func extractDirectoryListPath(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file_path", "directory", "dir"} {
		if raw, ok := payload[key]; ok {
			if path, ok := raw.(string); ok {
				return strings.TrimSpace(path)
			}
		}
	}
	return ""
}

func rewriteForeignAbsoluteToolPathInput(name, input, workdir string) string {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return input
	}
	nameKey := strings.ToLower(strings.TrimSpace(name))
	switch nameKey {
	case "read", "edit", "write", "glob", "grep":
	default:
		return input
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return input
	}

	changed := false
	for _, key := range []string{"file_path", "path", "directory", "dir"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		path, ok := raw.(string)
		if !ok {
			continue
		}
		rewritten := rebaseAbsolutePathToWorkdir(path, workdir)
		if rewritten != path {
			payload[key] = rewritten
			changed = true
		}
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

func rewriteForeignBashReadCommandInput(name, input, workdir string) string {
	if !strings.EqualFold(strings.TrimSpace(name), "bash") {
		return input
	}
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return input
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return input
	}

	command, _ := payload["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return input
	}
	if localized, ok := rewriteBashReadCandidatesToLocalSearch(command, workdir); ok {
		payload["command"] = localized
		normalized, err := json.Marshal(payload)
		if err != nil {
			return input
		}
		return string(normalized)
	}

	changed := false
	rewrittenCommand := quotedPathRegex.ReplaceAllStringFunc(command, func(match string) string {
		if len(match) < 2 {
			return match
		}
		pathValue := match[1 : len(match)-1]
		rewritten := rebaseCandidatePathToWorkdir(pathValue, workdir)
		if rewritten == pathValue {
			return match
		}
		changed = true
		return strconv.Quote(rewritten)
	})
	if !changed {
		return input
	}
	payload["command"] = rewrittenCommand
	normalized, err := json.Marshal(payload)
	if err != nil {
		return input
	}
	return string(normalized)
}

func rewriteBashReadCandidatesToLocalSearch(command, workdir string) (string, bool) {
	command = strings.TrimSpace(command)
	workdir = strings.TrimSpace(workdir)
	if command == "" || workdir == "" {
		return "", false
	}
	if !strings.Contains(command, "[ -f ") || !strings.Contains(command, "sed -n '1,240p'") {
		return "", false
	}

	matches := quotedPathRegex.FindAllStringSubmatch(command, -1)
	if len(matches) == 0 {
		return "", false
	}

	var basenames []string
	var exactCandidates []string
	seenBase := map[string]struct{}{}
	seenExact := map[string]struct{}{}
	needsLocalization := false

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		pathValue := strings.TrimSpace(match[1])
		if pathValue == "" {
			continue
		}
		base := filepath.Base(pathValue)
		if base == "" || base == "." || base == string(filepath.Separator) {
			continue
		}

		if filepath.IsAbs(pathValue) || strings.Contains(pathValue, string(filepath.Separator)) {
			if !sameOrWithinPath(pathValue, workdir) {
				needsLocalization = true
			}
		}

		rewritten := rebaseCandidatePathToWorkdir(pathValue, workdir)
		if rewritten != pathValue {
			needsLocalization = true
		}
		if strings.TrimSpace(rewritten) != "" && pathExists(rewritten) && sameOrWithinPath(rewritten, workdir) {
			if _, ok := seenExact[rewritten]; !ok {
				seenExact[rewritten] = struct{}{}
				exactCandidates = append(exactCandidates, rewritten)
			}
		}
		if _, ok := seenBase[base]; !ok {
			seenBase[base] = struct{}{}
			basenames = append(basenames, base)
		}
	}

	if !needsLocalization || len(basenames) == 0 {
		return "", false
	}

	var parts []string
	for _, candidate := range exactCandidates {
		quoted := strconv.Quote(candidate)
		parts = append(parts, "if [ -f "+quoted+" ]; then sed -n '1,240p' < "+quoted+"; exit 0; fi")
	}
	for _, base := range basenames {
		quotedBase := strconv.Quote(base)
		parts = append(parts, "found=$(find . -type f -name "+quotedBase+" | head -n 1)")
		parts = append(parts, "if [ -n \"$found\" ]; then sed -n '1,240p' < \"$found\"; exit 0; fi")
	}
	parts = append(parts, "echo 'File does not exist.'; exit 1")
	return strings.Join(parts, "; "), true
}

func rewriteAbsoluteReadToBashFallback(name, input, workdir string) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(name), "read") {
		return "", false
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return "", false
	}
	rawPath, _ := payload["file_path"].(string)
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" || !filepath.IsAbs(rawPath) {
		return "", false
	}
	if strings.TrimSpace(workdir) == "" {
		return "", false
	}
	if sameOrWithinPath(rawPath, workdir) {
		return "", false
	}

	candidates := relativeReadCandidates(rawPath)
	if len(candidates) == 0 {
		return "", false
	}

	var parts []string
	for _, candidate := range candidates {
		quoted := strconv.Quote(candidate)
		parts = append(parts, "if [ -f "+quoted+" ]; then sed -n '1,240p' < "+quoted+"; exit 0; fi")
	}
	command := strings.Join(parts, "; ") + "; echo 'File does not exist.'; exit 1"
	normalized, err := json.Marshal(map[string]string{
		"command":     command,
		"description": "Read likely local file by relative candidates",
	})
	if err != nil {
		return "", false
	}
	return string(normalized), true
}

func relativeReadCandidates(pathValue string) []string {
	parts := splitPathSegments(pathValue)
	if len(parts) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 6)
	maxKeep := 4
	if len(parts) < maxKeep {
		maxKeep = len(parts)
	}
	for keep := maxKeep; keep >= 1; keep-- {
		candidate := filepath.Join(parts[len(parts)-keep:]...)
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func rebaseCandidatePathToWorkdir(pathValue, workdir string) string {
	pathValue = strings.TrimSpace(pathValue)
	workdir = strings.TrimSpace(workdir)
	if pathValue == "" || workdir == "" {
		return pathValue
	}
	if filepath.IsAbs(pathValue) {
		return rebaseAbsolutePathToWorkdir(pathValue, workdir)
	}

	cleanWorkdir := filepath.Clean(workdir)
	projectBase := filepath.Base(cleanWorkdir)
	parts := splitPathSegments(pathValue)
	if len(parts) == 0 {
		return pathValue
	}

	for i, part := range parts {
		if !strings.EqualFold(strings.TrimSpace(part), projectBase) {
			continue
		}
		if i+1 >= len(parts) {
			break
		}
		candidate := filepath.Join(cleanWorkdir, filepath.Join(parts[i+1:]...))
		if pathExists(candidate) {
			return candidate
		}
	}

	maxKeep := 4
	if len(parts) < maxKeep {
		maxKeep = len(parts)
	}
	for keep := maxKeep; keep >= 1; keep-- {
		candidate := filepath.Join(cleanWorkdir, filepath.Join(parts[len(parts)-keep:]...))
		if pathExists(candidate) {
			return candidate
		}
	}

	base := filepath.Base(pathValue)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return pathValue
	}
	candidate := filepath.Join(cleanWorkdir, base)
	if pathExists(candidate) {
		return candidate
	}
	return pathValue
}

func rebaseAbsolutePathToWorkdir(pathValue, workdir string) string {
	pathValue = strings.TrimSpace(pathValue)
	workdir = strings.TrimSpace(workdir)
	if pathValue == "" || workdir == "" {
		return pathValue
	}
	if isPlaceholderDirectoryListPath(pathValue) {
		return workdir
	}
	if !filepath.IsAbs(pathValue) {
		return pathValue
	}

	cleanWorkdir := filepath.Clean(workdir)
	cleanPath := filepath.Clean(pathValue)
	if sameOrWithinPath(cleanPath, cleanWorkdir) {
		return pathValue
	}

	parts := splitPathSegments(cleanPath)
	maxKeep := 4
	if len(parts) < maxKeep {
		maxKeep = len(parts)
	}
	for keep := maxKeep; keep >= 1; keep-- {
		tail := filepath.Join(parts[len(parts)-keep:]...)
		candidate := filepath.Join(cleanWorkdir, tail)
		if pathExists(candidate) {
			return candidate
		}
	}

	base := filepath.Base(cleanPath)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return pathValue
	}
	candidate := filepath.Join(cleanWorkdir, base)
	if pathExists(candidate) {
		return candidate
	}
	return pathValue
}

func sameOrWithinPath(pathValue, root string) bool {
	pathValue = filepath.Clean(pathValue)
	root = filepath.Clean(root)
	if pathValue == root {
		return true
	}
	rel, err := filepath.Rel(root, pathValue)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func splitPathSegments(pathValue string) []string {
	pathValue = filepath.Clean(pathValue)
	parts := strings.FieldsFunc(pathValue, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func pathExists(pathValue string) bool {
	if strings.TrimSpace(pathValue) == "" {
		return false
	}
	_, err := os.Stat(pathValue)
	return err == nil
}

func isPlaceholderDirectoryListPath(path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}
	for len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	switch trimmed {
	case "/home/user/app":
		return true
	default:
		return false
	}
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

func (h *streamHandler) emitToolCallStream(call toolCall, idx int, final bool) {
	if call.id == "" {
		return
	}

	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	if inputJSON == "" {
		inputJSON = "{}"
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if idx < 0 {
		h.blockIndex++
		idx = h.blockIndex
	}
	h.writeSSEContentBlockStartToolUseLocked(idx, call.id, call.name, final)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, final)
	h.writeSSEContentBlockStopLocked(idx, final)
}

// emitToolUseFromInput emits a single tool_use block once the full input is available.
func (h *streamHandler) emitToolUseFromInput(toolID, toolName, inputStr string) {
	if toolID == "" || toolName == "" {
		return
	}
	if _, ok := h.toolCallEmitted[toolID]; ok {
		return
	}
	h.toolCallEmitted[toolID] = struct{}{}

	h.addOutputTokens(toolName)
	inputJSON := strings.TrimSpace(inputStr)
	if inputJSON == "" {
		inputJSON = "{}"
	}

	h.mu.Lock()
	h.toolCallCount++
	h.blockIndex++
	idx := h.blockIndex
	h.writeSSEContentBlockStartToolUseLocked(idx, toolID, toolName, false)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, false)
	h.writeSSEContentBlockStopLocked(idx, false)
	h.mu.Unlock()
}

func (h *streamHandler) flushPendingToolCalls(stopReason string) {
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
			h.emitToolCallStream(call, -1, true)
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

	// Ensure there's some text output before closing if we return end_turn with no output
	if stopReason != "tool_use" && h.noToolsFallbackText == "" && !h.suppressEmptyOutputFallback && !h.hasAnyOutput() {
		emptyMsg := "No output was presented to the user. This may be due to tool calls being suppressed or the model producing no text content."
		if h.isStream {
			h.emitTextBlockWithMode(emptyMsg, false)
		} else {
			h.mu.Lock()
			h.responseText.WriteString(emptyMsg)
			h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
				"type": "text",
				"text": emptyMsg,
			})
			h.hasTextOutput = true
			h.mu.Unlock()
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
		var blockStopData []byte
		h.mu.Lock()
		if stopData, ok := h.popActiveBlockStopDataLocked(); ok {
			blockStopData = stopData
		}
		h.mu.Unlock()
		if len(blockStopData) > 0 {
			h.writeFinalSSEBytes("content_block_stop", blockStopData)
		}
		if stopReason != "tool_use" {
			h.emitWriteChunkFallbackIfNeeded()
			h.emitNoToolsFallbackIfNeeded()
		}
		h.flushPendingToolCalls(stopReason)
		h.finalizeOutputTokens()
		h.mu.Lock()
		h.writeSSEMessageDeltaLocked(stopReason, h.outputTokens, true)
		h.mu.Unlock()

		stopData, err := marshalSSEMessageStopBytes()
		if err != nil {
			slog.Error("Failed to marshal message_stop", "error", err)
		} else {
			h.writeFinalSSEBytes("message_stop", stopData)
		}
	} else {
		if stopReason != "tool_use" {
			h.emitWriteChunkFallbackIfNeeded()
			h.emitNoToolsFallbackIfNeeded()
		}
		h.flushPendingToolCalls(stopReason)
		h.finalizeOutputTokens()
	}

	h.finalizeCompletion(stopReason)
}

func (h *streamHandler) finalizeCompletion(stopReason string) {
	h.mu.Lock()
	if h.completionLogged {
		h.mu.Unlock()
		return
	}
	h.completionLogged = true
	h.mu.Unlock()

	// 闂傚倷娴囧畷鍨叏閹惰姤鍊块柨鏇楀亾妞ゎ厼鐏濊灒闁兼祴鏅濋ˇ顖炴倵楠炲灝鍔氭い锔诲灣缁鎮滃Ο鍦畾濡炪倖鐗楁笟妤呭磿閵夛妇绠?
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

		raw, err := appendSSEContentBlockStartThinking(h.ssePayloadScratch[:0], sseIdx, signature)
		if err != nil {
			h.markWriteErrorLocked("content_block_start", err)
			break
		}
		h.ssePayloadScratch = raw[:0]
		h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
	case "text":
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type": "text",
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeTextBlockIndex = internalIdx
		h.activeTextSSEIndex = sseIdx
		h.textBlockBuilders[internalIdx] = perf.AcquireStringBuilder()

		h.writeSSEContentBlockStartTextLocked(sseIdx, false)
	}

	return sseIdx
}

func (h *streamHandler) closeActiveBlock() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeActiveBlockLocked()
}

func (h *streamHandler) popActiveBlockStopDataLocked() ([]byte, bool) {
	if h.activeBlockType == "" {
		return nil, false
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
		return nil, false
	}

	h.activeBlockType = ""

	stopData, err := marshalSSEContentBlockStopBytes(sseIdx)
	if err != nil {
		slog.Error("Failed to marshal content_block_stop", "error", err)
	}
	if err != nil {
		return nil, false
	}
	return stopData, true
}

func (h *streamHandler) closeActiveBlockLocked() {
	stopData, ok := h.popActiveBlockStopDataLocked()
	if !ok {
		return
	}
	h.writeSSEBytesLocked("content_block_stop", stopData)
}

func (h *streamHandler) writeSSELocked(event, data string) {
	if !h.isStream {
		return
	}
	if h.hasReturn {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSE(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSELocked(event, data, false)
		}
		return
	}
	if err := writeSSEFrame(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSELocked(event, data, false)
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, data)
	}
	// Log to slog only when debug enabled
	if h.config != nil && h.config.DebugEnabled {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}

func (h *streamHandler) writeSSEBytesLocked(event string, data []byte) {
	if !h.isStream {
		return
	}
	if h.hasReturn {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSEBytesLocked(event, data, false)
		}
		return
	}
	if err := writeSSEFrameBytes(h.w, event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSEBytesLocked(event, data, false)
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, string(data))
	}
	// Log to slog only when debug enabled
	if h.config != nil && h.config.DebugEnabled {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}

// Event Handlers

func (h *streamHandler) emitTextBlock(text string) {
	h.emitTextBlockWithMode(text, false)
}

func (h *streamHandler) emitTextBlockWithMode(text string, final bool) {
	if !h.isStream || text == "" {
		return
	}
	h.mu.Lock()
	h.hasTextOutput = true
	h.blockIndex++
	idx := h.blockIndex
	h.writeSSEContentBlockStartTextLocked(idx, final)
	h.writeSSEContentBlockDeltaTextLocked(idx, text, final)
	h.writeSSEContentBlockStopLocked(idx, final)
	h.mu.Unlock()
}

func (h *streamHandler) markTextOutput() {
	h.mu.Lock()
	h.hasTextOutput = true
	h.mu.Unlock()
}

func (h *streamHandler) emitWriteChunkFallbackIfNeeded() {
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
		h.emitTextBlockWithMode(text, true)
		return
	}

	h.mu.Lock()
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type": "text",
		"text": text,
	})
	h.mu.Unlock()
}

func (h *streamHandler) emitNoToolsFallbackIfNeeded() {
	h.mu.Lock()
	text := strings.TrimSpace(h.noToolsFallbackText)
	currentText := strings.TrimSpace(h.currentTextForNoToolsFallbackLocked())
	shouldEmit := text != "" &&
		h.suppressedToolCalls > 0 &&
		!h.hasTextOutput &&
		h.responseText.Len() == 0
	shouldAppend := text != "" &&
		looksLikeWeakNoToolsPreface(currentText) &&
		!strings.Contains(currentText, text)
	if shouldEmit {
		h.hasTextOutput = true
	}
	h.mu.Unlock()
	if !shouldEmit && !shouldAppend {
		return
	}

	if shouldAppend {
		if h.isStream {
			h.emitTextBlockWithMode(text, true)
			return
		}

		if h.responseText.Len() > 0 {
			h.responseText.WriteString("\n\n")
		}
		h.responseText.WriteString(text)
		h.mu.Lock()
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type": "text",
			"text": text,
		})
		h.mu.Unlock()
		return
	}

	if h.isStream {
		h.emitTextBlockWithMode(text, true)
		return
	}

	h.responseText.WriteString(text)
	h.mu.Lock()
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type": "text",
		"text": text,
	})
	h.mu.Unlock()
}

func (h *streamHandler) currentTextForNoToolsFallbackLocked() string {
	if !h.isStream && h.responseText.Len() > 0 {
		return h.responseText.String()
	}

	var parts []string
	for idx, block := range h.contentBlocks {
		blockType, _ := block["type"].(string)
		if blockType != "text" {
			continue
		}
		if builder, ok := h.textBlockBuilders[idx]; ok {
			if text := strings.TrimSpace(builder.String()); text != "" {
				parts = append(parts, text)
				continue
			}
		}
		if text, ok := block["text"].(string); ok {
			text = strings.TrimSpace(text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func looksLikeWeakNoToolsPreface(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if len([]rune(text)) > 220 {
		return false
	}

	lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
	intro := []string{
		"let me",
		"i'll first",
		"i will first",
		"让我先",
		"我先",
		"let我先",
	}
	action := []string{
		"look",
		"read",
		"explore",
		"examine",
		"analyze",
		"identify",
		"understand",
		"inspect",
		"check",
		"learn",
		"看看",
		"看一下",
		"了解",
		"阅读",
		"读取",
		"理解",
	}

	hasIntro := false
	for _, marker := range intro {
		if strings.Contains(lower, marker) {
			hasIntro = true
			break
		}
	}
	if !hasIntro {
		return false
	}
	for _, marker := range action {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (h *streamHandler) handleToolCallAfterChecks(call toolCall) {
	h.mu.Lock()
	h.pendingToolCalls = append(h.pendingToolCalls, call)
	h.toolCallCount++
	h.mu.Unlock()
}

func (h *streamHandler) shouldAcceptToolCall(call toolCall) bool {
	h.mu.Lock()
	disallowToolCalls := h.disallowToolCalls
	allowedTool := true
	if len(h.allowedToolNames) > 0 {
		lowerName := strings.ToLower(strings.TrimSpace(call.name))
		_, allowedTool = h.allowedToolNames[lowerName]
		if !allowedTool {
			// Auto-allow meta tools
			if lowerName == "todowrite" || lowerName == "taskoutput" || lowerName == "taskstop" ||
				lowerName == "write" || lowerName == "read" || lowerName == "ls" || lowerName == "grep" {
				allowedTool = true
			}
		}
	}
	if disallowToolCalls {
		h.suppressedToolCalls++
	}
	if !allowedTool {
		h.suppressedToolCalls++
	}
	h.mu.Unlock()
	if disallowToolCalls {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed by no-tools gate", "tool", call.name, "input", call.input)
		}
		return false
	}
	if !allowedTool {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed because it is not declared in the current request", "tool", call.name, "input", call.input)
		}
		return false
	}

	_, key, ok := evaluateToolCallInput(call.name, call.input)
	if !ok {
		h.mu.Lock()
		h.suppressedToolCalls++
		h.mu.Unlock()
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("invalid tool call suppressed", "tool", call.name, "input", call.input)
		}
		return false
	}
	if key != "" {
		maskedKey := maskDedupKey(key)
		h.mu.Lock()
		if _, ok := h.bashCallDedup[key]; ok {
			h.toolDedupCount++
			h.toolDedupKeys[maskedKey]++
			h.suppressedToolCalls++
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
	sum := fnv1a64String(key)
	out := make([]byte, 0, len(tool)+1+16)
	out = append(out, tool...)
	out = append(out, '#')
	out = strconv.AppendUint(out, sum, 16)
	return string(out)
}

func sideEffectToolDedupKey(name, input string) string {
	nameKey := normalizeToolNameKey(name)
	if !isSideEffectToolName(nameKey) {
		return ""
	}
	fields, ok := decodeToolInputFields(input)
	if !ok {
		return ""
	}
	return sideEffectToolDedupKeyFromFields(nameKey, fields)
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
	sum := fnv1a64Pair(nameKey, normalizedInput)
	out := make([]byte, 0, len("tool_anon_")+16)
	out = append(out, "tool_anon_"...)
	out = strconv.AppendUint(out, sum, 16)
	return string(out)
}

func hasRequiredToolInput(name, input string) bool {
	nameKey := normalizeToolNameKey(name)
	if nameKey == "" {
		return false
	}
	if !isStructuredToolName(nameKey) {
		return true
	}
	fields, ok := decodeToolInputFields(input)
	if !ok {
		return false
	}
	return hasRequiredToolInputFields(nameKey, fields)
}

func evaluateToolCallInput(name, input string) (nameKey string, dedupKey string, ok bool) {
	nameKey = normalizeToolNameKey(name)
	if nameKey == "" {
		return "", "", false
	}
	if !isStructuredToolName(nameKey) {
		return nameKey, "", true
	}
	fields, parsed := decodeToolInputFields(input)
	if !parsed {
		return nameKey, "", false
	}
	if !hasRequiredToolInputFields(nameKey, fields) {
		return nameKey, "", false
	}
	return nameKey, sideEffectToolDedupKeyFromFields(nameKey, fields), true
}

func normalizeToolNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func isStructuredToolName(nameKey string) bool {
	switch nameKey {
	case "edit", "write", "bash", "read", "glob", "grep":
		return true
	default:
		return false
	}
}

func isSideEffectToolName(nameKey string) bool {
	switch nameKey {
	case "bash", "write", "edit":
		return true
	default:
		return false
	}
}

type toolInputFields struct {
	Command    string          `json:"command"`
	Cmd        string          `json:"cmd"`
	FilePath   string          `json:"file_path"`
	Path       string          `json:"path"`
	Content    json.RawMessage `json:"content"`
	Old        json.RawMessage `json:"old_string"`
	New        json.RawMessage `json:"new_string"`
	IsReadOnly *bool           `json:"is_read_only"`
	IsRisky    *bool           `json:"is_risky"`
}

func decodeToolInputFields(input string) (toolInputFields, bool) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		raw = "{}"
	}
	var fields toolInputFields
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return toolInputFields{}, false
	}
	return fields, true
}

func resolveToolPath(filePath, path string) string {
	if s := strings.TrimSpace(filePath); s != "" {
		return s
	}
	if s := strings.TrimSpace(path); s != "" {
		return s
	}
	return ""
}

func hasRequiredToolInputFields(nameKey string, fields toolInputFields) bool {
	switch nameKey {
	case "edit":
		path := resolveToolPath(fields.FilePath, fields.Path)
		return path != "" && len(fields.Old) > 0 && len(fields.New) > 0
	case "write":
		// Warp sometimes sends "path" instead of "file_path", or we might have mapped it.
		// Also strict checking might fail if "content" is empty string (though rare for meaningful write).
		path := resolveToolPath(fields.FilePath, fields.Path)
		return path != "" && len(fields.Content) > 0
	case "bash":
		return strings.TrimSpace(fields.Command) != "" || strings.TrimSpace(fields.Cmd) != ""
	case "read":
		return resolveToolPath(fields.FilePath, fields.Path) != ""
	default:
		return true
	}
}

func canonicalToolRawValue(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return trimmed
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return trimmed
	}
	return string(normalized)
}

func sideEffectToolDedupKeyFromFields(nameKey string, fields toolInputFields) string {
	if !isSideEffectToolName(nameKey) {
		return ""
	}
	switch nameKey {
	case "bash":
		command := strings.TrimSpace(fields.Command)
		if strings.TrimSpace(command) == "" {
			command = strings.TrimSpace(fields.Cmd)
		}
		command = strings.TrimSpace(command)
		if command == "" {
			return ""
		}
		if isReadOnlyBashCommand(command, fields.IsReadOnly, fields.IsRisky) {
			return ""
		}
		return "bash:" + command
	case "write":
		path := resolveToolPath(fields.FilePath, fields.Path)
		if path == "" {
			return ""
		}
		if len(fields.Content) == 0 {
			return ""
		}
		return "write:" + path + "\x00" + canonicalToolRawValue(fields.Content)
	case "edit":
		path := resolveToolPath(fields.FilePath, fields.Path)
		if path == "" {
			return ""
		}
		if len(fields.Old) == 0 || len(fields.New) == 0 {
			return ""
		}
		return "edit:" + path + "\x00" + canonicalToolRawValue(fields.Old) + "\x00" + canonicalToolRawValue(fields.New)
	default:
		return ""
	}
}

func isReadOnlyBashCommand(command string, isReadOnly, isRisky *bool) bool {
	if isRisky != nil && *isRisky {
		return false
	}
	if isReadOnly != nil {
		return *isReadOnly
	}

	lower := strings.ToLower(strings.TrimSpace(command))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, " -exec ") || strings.Contains(lower, " -delete") {
		return false
	}
	if strings.Contains(lower, "&&") || strings.Contains(lower, "||") || strings.Contains(lower, ";") {
		return false
	}
	if strings.Contains(lower, ">") || strings.Contains(lower, "<") {
		return false
	}

	segments := strings.Split(lower, "|")
	for _, segment := range segments {
		fields := strings.Fields(strings.TrimSpace(segment))
		if len(fields) == 0 {
			return false
		}
		cmd := fields[0]
		switch cmd {
		case "find", "sort", "ls", "pwd", "cat", "head", "tail", "grep", "rg", "wc", "stat", "file", "tree", "du", "basename", "dirname", "realpath", "readlink", "which", "type", "fd":
			continue
		case "sed":
			if len(fields) > 1 && fields[1] == "-n" {
				continue
			}
			return false
		default:
			return false
		}
	}
	return true
}

func fnv1a64String(s string) uint64 {
	h := fnv64Offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnv64Prime
	}
	return h
}

func fnv1a64Pair(a, b string) uint64 {
	h := fnv64Offset
	for i := 0; i < len(a); i++ {
		h ^= uint64(a[i])
		h *= fnv64Prime
	}
	h ^= 0
	h *= fnv64Prime
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= fnv64Prime
	}
	return h
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
	slog.Warn("SSE write failed", "event", event, "error", err)
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
	hasOutput := h.hasTextOutput || h.responseText.Len() > 0 || len(h.contentBlocks) > 0
	h.mu.Unlock()

	// Inject a fallback text block if upstream produced nothing.
	if !hasToolCalls && !hasOutput {
		slog.Warn("Upstream returned no output; injecting fallback text block")
		h.ensureBlock("text")
		h.mu.Lock()
		internalIdx := h.activeTextBlockIndex
		sseIdx := h.activeTextSSEIndex
		h.mu.Unlock()

		emptyMsg := "No response from upstream. The request may not be supported in this mode."
		if h.isStream {
			deltaData, _ := marshalSSEContentBlockDeltaTextBytes(sseIdx, emptyMsg)
			h.writeSSEBytes("content_block_delta", deltaData)
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
	slog.Warn("Upstream stream ended without explicit stop marker; forcing response finish", "stop_reason", stopReason)
	h.finishResponse(stopReason)
}

func (h *streamHandler) hasAnyOutput() bool {
	h.mu.Lock()
	has := h.hasTextOutput ||
		h.toolCallCount > 0 ||
		len(h.pendingToolCalls) > 0 ||
		len(h.toolCallEmitted) > 0 ||
		len(h.contentBlocks) > 0 ||
		h.responseText.Len() > 0 ||
		h.writeChunkBuffer.Len() > 0
	h.mu.Unlock()
	if has {
		return true
	}

	h.outputMu.Lock()
	has = h.outputEstimator.HasText() || (h.outputTokens-h.thinkingTokens) > 0
	h.outputMu.Unlock()
	return has
}

func (h *streamHandler) hasReturnedResponse() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hasReturn
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

func (h *streamHandler) shouldSkipCrossChannelDuplicateDelta(source, delta string) bool {
	if strings.TrimSpace(delta) == "" || source == "" {
		return false
	}
	now := time.Now()

	h.mu.Lock()
	defer h.mu.Unlock()

	skip := h.lastTextDelta == delta &&
		h.lastTextDeltaSource != "" &&
		h.lastTextDeltaSource != source &&
		now.Sub(h.lastTextDeltaAt) <= 2*time.Second

	h.lastTextDelta = delta
	h.lastTextDeltaSource = source
	h.lastTextDeltaAt = now
	return skip
}

func normalizeIntroKey(delta string) string {
	text := strings.TrimSpace(delta)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	compactLower := strings.Join(strings.Fields(strings.ReplaceAll(lower, "\U0001F44B", "")), " ")
	switch lower {
	case "hi! how can i help you today?",
		"hello! how can i help you today?",
		"hi! how can i help you today!",
		"hello! how can i help you today!":
		return "intro:en:greet"
	}
	switch compactLower {
	case "hi! what's up? how can i help today?",
		"hello! what's up? how can i help today?",
		"hi! how can i help today?",
		"hello! how can i help today?",
		"hi! how can i help you today?",
		"hello! how can i help you today?",
		"hi! how can i help you today!",
		"hello! how can i help you today!":
		return "intro:en:greet"
	}
	if (strings.HasPrefix(compactLower, "hi!") || strings.HasPrefix(compactLower, "hello!") || strings.HasPrefix(compactLower, "hey!")) &&
		(strings.Contains(compactLower, "how can i help today") || strings.Contains(compactLower, "how can i help you today")) {
		return "intro:en:greet"
	}
	if strings.HasPrefix(text, "\u4f60\u597d") || strings.HasPrefix(text, "\u60a8\u597d") || strings.Contains(text, "\u6211\u80fd\u5e2e\u4f60") {
		return "intro:zh:greet"
	}
	if strings.Contains(lower, "warp") && (strings.HasPrefix(text, "\u6211\u662f") || strings.Contains(text, "agent mode")) {
		return "intro:zh:warp"
	}
	if strings.Contains(lower, "claude") && (strings.HasPrefix(text, "\u6211\u662f") || strings.Contains(lower, "claude 4")) {
		return "intro:zh:claude"
	}
	return ""
}

func collapseDuplicatedIntroDelta(delta string) string {
	text := strings.TrimSpace(delta)
	if text == "" || len(text)%2 != 0 {
		return delta
	}
	half := len(text) / 2
	first := strings.TrimSpace(text[:half])
	second := strings.TrimSpace(text[half:])
	if first == "" || second == "" || first != second {
		return delta
	}
	if normalizeIntroKey(first) == "" {
		return delta
	}
	return first
}

// extractThinkingSignature extracts a signature from event or event.data.
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
		fields := []any{"type", msg.Type}
		if msg.Event != nil {
			// Avoid leaking secrets in logs: only log high-level shape.
			evtType, _ := msg.Event["type"].(string)
			fields = append(fields, "event_type", evtType)
			if delta, ok := msg.Event["delta"]; ok {
				fields = append(fields, "has_delta", delta != nil)
			}
			if data, ok := msg.Event["data"].(map[string]interface{}); ok {
				fields = append(fields, "data_keys", mapKeys(data))
				if msgStr, ok := data["message"].(string); ok {
					fields = append(fields, "data_message_len", len(msgStr))
				}
			}
			fields = append(fields, "event_keys", mapKeys(msg.Event))
		}
		slog.Debug("Incoming SSE", fields...)
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

	if h.handleDirectFinalSSEEvent(msg) {
		return
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
			h.addThinkingTokens(delta)
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
		h.writeSSEContentBlockDeltaThinking(sseIdx, delta, false)

	case "model.reasoning-end":
		h.closeActiveBlock()

	case "model.text-start":
		h.ensureBlock("text")

	case "model.text-delta", "coding_agent.output_text.delta":
		delta := ""
		source := eventKey
		if msg.Type == "model" {
			delta, _ = msg.Event["delta"].(string)
		} else {
			// coding_agent.output_text.delta
			delta, _ = msg.Event["delta"].(string)
		}
		if delta == "" {
			return
		}
		delta = collapseDuplicatedIntroDelta(delta)

		// Parse Orchids tool markers hidden in the text delta
		if matches := orchidsToolMarkerRegex.FindStringSubmatch(delta); len(matches) > 2 {
			toolName := matches[1]
			toolInput := matches[2]
			if h.config != nil && h.config.DebugEnabled {
				slog.Debug("Orchids tool marker detected in text delta", "tool", toolName, "input", toolInput)
			}
			// Emit it as a structured tool call
			h.handleMessage(upstream.SSEMessage{
				Type: "model.tool-call",
				Event: map[string]interface{}{
					"toolName":   toolName,
					"toolCallId": fmt.Sprintf("call_%d", time.Now().UnixNano()),
					"input":      toolInput,
				},
			})
			// If it's the exact content of the delta, we can skip the text delta entirely
			if strings.TrimSpace(delta) == strings.TrimSpace(matches[0]) {
				return
			}
			// Otherwise, remove the marker from the text
			delta = strings.ReplaceAll(delta, matches[0], "")
			if strings.TrimSpace(delta) == "" {
				return
			}
		}

		if h.shouldSkipIntroDelta(delta) {
			return
		}
		if h.shouldSkipCrossChannelDuplicateDelta(source, delta) {
			if h.config != nil && h.config.DebugEnabled {
				slog.Debug("skip cross-channel duplicate delta", "source", source, "delta_len", len(delta))
			}
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
		h.writeSSEContentBlockDeltaText(sseIdx, delta, false)

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
		h.writeUpstreamEventSSE(msg)
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

				h.writeUpstreamEventSSE(msg)
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
				h.writeUpstreamEventSSE(msg)
			}
		}
		return

	case "coding_agent.Write.content.completed", "coding_agent.Edit.edit.completed", "coding_agent.edit_file.completed":
		if h.isStream {
			if !h.suppressThinking {
				h.emitThinkingDelta("\n[Done]\n")
				h.writeUpstreamEventSSE(msg)
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
			h.writeUpstreamEventSSE(msg)
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
		toolName = strings.TrimSpace(toolName)
		if toolID == "" || toolName == "" {
			return
		}
		h.currentToolInputID = toolID
		h.toolInputNames[toolID] = toolName
		h.toolInputBuffers[toolID] = perf.AcquireStringBuilder()
		h.toolInputHadDelta[toolID] = false
		// 婵犵數濮烽弫鎼佸磻濞戔懞鍥敇閵忕姷顦悗骞垮劚椤︻垳绮堥崼婢濆綊鎮℃惔锝嗘喖闂佸搫鎷嬮崜姘跺箞閵娿儺娼ㄩ柛鈩冦仦缁ㄤ粙姊洪懡銈呮瀾缂佽鐗撻獮鍐倻閽樺宓嗗┑顔斤耿绾危椤斿皷鏀介柣姗嗗亜娴?tool-input-start 闂傚倷娴囬褏鎹㈤幇顔藉床闁归偊鍓涢弳锔姐亜閹烘垵鏆斿ù婊冪秺閺屾稑鐣濋埀顒勫磻閻愮儤鍊?tool_use闂傚倸鍊烽悞锔锯偓绗涘懐鐭欓柟杈鹃檮閸庢鏌涚仦鍓р槈妞ゆ洟浜堕弻宥夊传閸曨剙娅ｇ紓浣插亾闁稿本澹曢崑鎾荤嵁閸喖濮庨柣搴㈠嚬閸ｏ綁骞冮悜钘夌疀妞ゆ挾濮烽鏇㈡⒑閻熸澘鈷旂紒顕呭灠閳诲秴顭ㄩ崼鐔哄幘闂佸壊鐓堥崑鍕倶鐎电硶鍋撳▓鍨珮闁告挾鍠栭妴浣割潨閳ь剟骞冨鍫濆耿婵°倓绶￠崯宀勬⒒閸屾瑨鍏岄柛妯犲洤搴婇柡灞诲劜閸嬨倝鏌曟繛鍨壔?tool_result闂?		return

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
		name, inputStr = normalizeUpstreamToolCall(name, inputStr, h.workdir)
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
		toolName, inputStr = normalizeUpstreamToolCall(toolName, inputStr, h.workdir)
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

	h.writeSSEContentBlockDeltaThinking(sseIdx, delta, false)
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

	h.writeSSEContentBlockDeltaText(sseIdx, delta, false)
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
		data, _ := marshalSSEContentBlockDeltaTextBytes(idx, errorMsg)
		h.writeSSEBytes("content_block_delta", data)
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
