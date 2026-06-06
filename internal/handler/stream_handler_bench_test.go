package handler

import (
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/perf"
	"orchids-api/internal/upstream"
)

type discardStringByteWriter struct{}

type discardFlushResponseWriter struct {
	header http.Header
}

func (discardStringByteWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discardStringByteWriter) WriteString(s string) (int, error) {
	return len(s), nil
}

func newDiscardFlushResponseWriter() *discardFlushResponseWriter {
	return &discardFlushResponseWriter{header: make(http.Header)}
}

func (w *discardFlushResponseWriter) Header() http.Header {
	return w.header
}

func (w *discardFlushResponseWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *discardFlushResponseWriter) WriteHeader(statusCode int) {}

func (w *discardFlushResponseWriter) Flush() {}

func newBenchmarkStreamHandler(b *testing.B) *streamHandler {
	b.Helper()
	cfg := &config.Config{DebugEnabled: false}
	logger := debug.New(false, false)
	b.Cleanup(func() { logger.Close() })
	sh := newStreamHandler(cfg, newDiscardFlushResponseWriter(), logger, false, true, adapter.FormatAnthropic, "")
	b.Cleanup(sh.release)
	return sh
}

func writeSSEFrameBytesMultiWrite(w io.Writer, event string, data []byte) error {
	if _, err := io.WriteString(w, sseEventPrefix); err != nil {
		return err
	}
	if _, err := io.WriteString(w, event); err != nil {
		return err
	}
	if _, err := io.WriteString(w, sseDataJoin); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseLineBreak)
	return err
}

func writeOpenAIFrameMultiWrite(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, sseDataPrefix); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseLineBreak)
	return err
}

func writeSSEFrameBytesBuffered(w io.Writer, event string, data []byte) error {
	totalLen := len(sseEventPrefix) + len(event) + len(sseDataJoin) + len(data) + len(sseLineBreak)
	if totalLen <= 4096 {
		buf := perf.AcquireByteBuffer()
		defer perf.ReleaseByteBuffer(buf)
		buf.Grow(totalLen)
		_, _ = buf.WriteString(sseEventPrefix)
		_, _ = buf.WriteString(event)
		_, _ = buf.WriteString(sseDataJoin)
		_, _ = buf.Write(data)
		_, _ = buf.WriteString(sseLineBreak)
		_, err := w.Write(buf.Bytes())
		return err
	}
	return writeSSEFrameBytesMultiWrite(w, event, data)
}

func writeOpenAIFrameBuffered(w io.Writer, payload []byte) error {
	totalLen := len(sseDataPrefix) + len(payload) + len(sseLineBreak)
	if totalLen <= 4096 {
		buf := perf.AcquireByteBuffer()
		defer perf.ReleaseByteBuffer(buf)
		buf.Grow(totalLen)
		_, _ = buf.WriteString(sseDataPrefix)
		_, _ = buf.Write(payload)
		_, _ = buf.WriteString(sseLineBreak)
		_, err := w.Write(buf.Bytes())
		return err
	}
	return writeOpenAIFrameMultiWrite(w, payload)
}

func BenchmarkMaskDedupKey(b *testing.B) {
	key := "bash:echo hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = maskDedupKey(key)
	}
}

func BenchmarkHasRequiredToolInputUnknownMalformed(b *testing.B) {
	tool := "UnknownTool"
	input := `{"bad":`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = hasRequiredToolInput(tool, input)
	}
}

func BenchmarkToolValidationAndDedupWrite_Separate(b *testing.B) {
	tool := "Write"
	nameKey := "write"
	input := `{"file_path":"/tmp/a.txt","content":"hello"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !hasRequiredToolInput(tool, input) {
			b.Fatal("unexpected invalid input")
		}
		_ = sideEffectToolDedupKey(nameKey, input)
	}
}

func BenchmarkToolValidationAndDedupWrite_Combined(b *testing.B) {
	tool := "Write"
	input := `{"file_path":"/tmp/a.txt","content":"hello"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, ok := evaluateToolCallInput(tool, input)
		if !ok {
			b.Fatal("unexpected invalid input")
		}
	}
}

func BenchmarkMarshalContentBlockDeltaText_MapStyle(b *testing.B) {
	idx := 7
	text := "hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := perf.AcquireMap()
		m["type"] = "content_block_delta"
		m["index"] = idx
		delta := perf.AcquireMap()
		delta["type"] = "text_delta"
		delta["text"] = text
		m["delta"] = delta
		raw, _ := json.Marshal(m)
		_ = string(raw)
		perf.ReleaseMap(delta)
		perf.ReleaseMap(m)
	}
}

func BenchmarkMarshalContentBlockDeltaText_StructStyle(b *testing.B) {
	idx := 7
	text := "hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaText(idx, text)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaText_Bytes(b *testing.B) {
	idx := 7
	text := "hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaTextBytes(idx, text)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaThinking_StructStyle(b *testing.B) {
	idx := 7
	thinking := "analyzing next step"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaThinking(idx, thinking)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaThinking_Bytes(b *testing.B) {
	idx := 7
	thinking := "analyzing next step"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaThinkingBytes(idx, thinking)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaInputJSON_String(b *testing.B) {
	idx := 5
	partialJSON := `{"file_path":"/tmp/a.txt","content":"hello world"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaInputJSON(idx, partialJSON)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaInputJSON_Bytes(b *testing.B) {
	idx := 5
	partialJSON := `{"file_path":"/tmp/a.txt","content":"hello world"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaInputJSONBytes(idx, partialJSON)
		_ = raw
	}
}

func BenchmarkMarshalMessageStart_Bytes(b *testing.B) {
	msgID := "msg_123"
	model := "claude-3-7-sonnet"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEMessageStartBytes(msgID, model, 12, 0)
		_ = raw
	}
}

func BenchmarkAppendSSEMessageStart_ReusedBuffer(b *testing.B) {
	msgID := "msg_123"
	model := "claude-3-7-sonnet"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEMessageStart(buf[:0], msgID, model, 12, 0)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaText_ReusedBuffer(b *testing.B) {
	idx := 7
	text := "hello world"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaText(buf[:0], idx, text)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaThinking_ReusedBuffer(b *testing.B) {
	idx := 7
	thinking := "analyzing next step"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaThinking(buf[:0], idx, thinking)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaInputJSON_ReusedBuffer(b *testing.B) {
	idx := 5
	partialJSON := `{"file_path":"/tmp/a.txt","content":"hello world"}`
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaInputJSON(buf[:0], idx, partialJSON)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkEmitTextBlock_Stream(b *testing.B) {
	sh := newBenchmarkStreamHandler(b)
	text := "hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sh.blockIndex = -1
		sh.emitTextBlock(text)
	}
}

func BenchmarkEmitToolCallStream_Final(b *testing.B) {
	sh := newBenchmarkStreamHandler(b)
	call := toolCall{
		id:    "tool_123",
		name:  "Write",
		input: `{"file_path":"/tmp/a.txt","content":"hello world"}`,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sh.blockIndex = -1
		sh.emitToolCallStream(call, -1, true)
	}
}

func BenchmarkWriteSSEMessageStart_Stream(b *testing.B) {
	sh := newBenchmarkStreamHandler(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sh.writeSSEMessageStart("claude-3-7-sonnet", 12, 0)
	}
}

func BenchmarkWriteSSEFrameBytes_MultiWrite(b *testing.B) {
	writer := discardStringByteWriter{}
	event := "content_block_delta"
	data := []byte("{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeSSEFrameBytesMultiWrite(writer, event, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteSSEFrameBytes_Buffered(b *testing.B) {
	writer := discardStringByteWriter{}
	event := "content_block_delta"
	data := []byte("{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeSSEFrameBytesBuffered(writer, event, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteOpenAIFrame_MultiWrite(b *testing.B) {
	writer := discardStringByteWriter{}
	payload := []byte("{\"id\":\"msg_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeOpenAIFrameMultiWrite(writer, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteOpenAIFrame_Buffered(b *testing.B) {
	writer := discardStringByteWriter{}
	payload := []byte("{\"id\":\"msg_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeOpenAIFrameBuffered(writer, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendJSONBytes_RawASCII(b *testing.B) {
	value := "hello world"
	dst := make([]byte, 0, len(value)+2)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendJSONBytes(dst[:0], value)
		_ = raw
	}
}

func BenchmarkAppendJSONBytes_EscapedJSONLike(b *testing.B) {
	value := "{\"file_path\":\"/tmp/a.txt\",\"content\":\"hello \\\"world\\\"\\nnext\"}"
	dst := make([]byte, 0, len(value)+16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendJSONBytes(dst[:0], value)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartToolUse_MapStyle(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		startMap := perf.AcquireMap()
		startMap["type"] = "content_block_start"
		startMap["index"] = idx
		contentBlock := perf.AcquireMap()
		contentBlock["type"] = "tool_use"
		contentBlock["id"] = id
		contentBlock["name"] = name
		contentBlock["input"] = perf.AcquireMap()
		startMap["content_block"] = contentBlock
		raw, _ := json.Marshal(startMap)
		_ = string(raw)
		perf.ReleaseMap(contentBlock["input"].(map[string]interface{}))
		perf.ReleaseMap(contentBlock)
		perf.ReleaseMap(startMap)
	}
}

func BenchmarkMarshalContentBlockStartToolUse_StructStyle(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartToolUse(idx, id, name)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartToolUse_Bytes(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartToolUseBytes(idx, id, name)
		_ = raw
	}
}

func BenchmarkAppendSSEContentBlockStartToolUse_ReusedBuffer(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockStartToolUse(buf[:0], idx, id, name)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkMarshalContentBlockStartText_String(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartText(idx)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartText_Bytes(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartTextBytes(idx)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartThinking_String(b *testing.B) {
	idx := 3
	signature := "sig_123"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartThinking(idx, signature)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartThinking_Bytes(b *testing.B) {
	idx := 3
	signature := "sig_123"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartThinkingBytes(idx, signature)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStop_String(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStop(idx)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStop_Bytes(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStopBytes(idx)
		_ = raw
	}
}

func BenchmarkMarshalMessageDelta_String(b *testing.B) {
	stopReason := "tool_use"
	outputTokens := 42
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEMessageDelta(stopReason, outputTokens)
		_ = raw
	}
}

func BenchmarkMarshalMessageDelta_Bytes(b *testing.B) {
	stopReason := "tool_use"
	outputTokens := 42
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEMessageDeltaBytes(stopReason, outputTokens)
		_ = raw
	}
}

func BenchmarkAppendSSEMessageDelta_ReusedBuffer(b *testing.B) {
	stopReason := "tool_use"
	outputTokens := 42
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEMessageDelta(buf[:0], stopReason, outputTokens)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkMarshalEventPayload_MapStyle(b *testing.B) {
	msg := upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"type": "coding_agent.Write.content.chunk",
			"data": map[string]interface{}{
				"file_path": "/tmp/a.txt",
				"text":      "hello world",
			},
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalEventPayload(msg)
		_ = raw
	}
}

func BenchmarkMarshalEventPayload_RawJSON(b *testing.B) {
	msg := upstream.SSEMessage{
		Type:    "coding_agent.Write.content.chunk",
		RawJSON: json.RawMessage(`{"type":"coding_agent.Write.content.chunk","data":{"file_path":"/tmp/a.txt","text":"hello world"}}`),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalEventPayload(msg)
		_ = raw
	}
}

func BenchmarkMarshalEventPayloadBytes_MapStyle(b *testing.B) {
	msg := upstream.SSEMessage{
		Type: "coding_agent.Write.content.chunk",
		Event: map[string]interface{}{
			"type": "coding_agent.Write.content.chunk",
			"data": map[string]interface{}{
				"file_path": "/tmp/a.txt",
				"text":      "hello world",
			},
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalEventPayloadBytes(msg)
		_ = raw
	}
}

func BenchmarkMarshalEventPayloadBytes_RawJSON(b *testing.B) {
	msg := upstream.SSEMessage{
		Type:    "coding_agent.Write.content.chunk",
		RawJSON: json.RawMessage(`{"type":"coding_agent.Write.content.chunk","data":{"file_path":"/tmp/a.txt","text":"hello world"}}`),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalEventPayloadBytes(msg)
		_ = raw
	}
}

func BenchmarkSanitizeToolInput_NoOpUnknown(b *testing.B) {
	input := `{"foo":"bar","n":1}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = sanitizeToolInput("unknown", input)
	}
}

func BenchmarkSanitizeToolInput_WriteMap(b *testing.B) {
	input := `{"path":"/tmp/a.txt","content":"hello","overwrite":true}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = sanitizeToolInput("Write", input)
	}
}

func BenchmarkSanitizeToolInput_WriteAlreadyNormalized(b *testing.B) {
	input := `{"file_path":"/tmp/a.txt","content":"hello"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = sanitizeToolInput("Write", input)
	}
}
