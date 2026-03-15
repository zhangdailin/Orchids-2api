package orchids

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
)

func legacyOrchidsSSEDataPayload(eventData string) (string, bool) {
	lines := strings.Split(eventData, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, orchidsSSEDataPrefix) {
			rawData := strings.TrimPrefix(line, orchidsSSEDataPrefix)
			if rawData == "" {
				return "", false
			}
			return rawData, true
		}
	}
	return "", false
}

func TestOrchidsSSEDataPayloadBytes(t *testing.T) {
	tests := []struct {
		name string
		line []byte
		want string
		ok   bool
	}{
		{name: "json line", line: []byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello\"}\n"), want: "{\"type\":\"output_text_delta\",\"text\":\"hello\"}", ok: true},
		{name: "json line crlf", line: []byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello\"}\r\n"), want: "{\"type\":\"output_text_delta\",\"text\":\"hello\"}", ok: true},
		{name: "blank data", line: []byte("data: \n"), want: "", ok: false},
		{name: "event header", line: []byte("event: message\n"), want: "", ok: false},
		{name: "blank line", line: []byte("\n"), want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := orchidsSSEDataPayloadBytes(tt.line)
			if ok != tt.ok || string(got) != tt.want {
				t.Fatalf("orchidsSSEDataPayloadBytes(%q) = (%q, %v), want (%q, %v)", tt.line, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestOrchidsSSEDataPayloadMatchesLegacySingleLineEvent(t *testing.T) {
	eventData := "event: message\ndata: {\"type\":\"output_text_delta\",\"text\":\"hello\"}\n\n"
	legacy, legacyOK := legacyOrchidsSSEDataPayload(eventData)
	if !legacyOK {
		t.Fatal("expected legacy parser to find data payload")
	}

	direct, directOK := orchidsSSEDataPayloadBytes([]byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello\"}\n"))
	if !directOK {
		t.Fatal("expected direct parser to find data payload")
	}
	if string(direct) != legacy {
		t.Fatalf("direct payload = %q, want %q", string(direct), legacy)
	}
}

func BenchmarkOrchidsSSEDataPayload_DirectBytes(b *testing.B) {
	line := []byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello world\"}\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = orchidsSSEDataPayloadBytes(line)
	}
}

func collectOrchidsEvents(client *Client, state *requestState, raw string, fast bool) ([]upstream.SSEMessage, bool, bool) {
	events := make([]upstream.SSEMessage, 0, 2)
	onMessage := func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}
	runtime := newTestRequestRuntime(*state, onMessage)
	rawBytes := []byte(raw)
	if fast {
		handled, shouldBreak := runtime.handleRawMessage(rawBytes, nil, nil)
		return events, handled, shouldBreak
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(rawBytes, &msg); err != nil {
		panic(err)
	}
	return events, true, runtime.handleDecodedMessage(msg, rawBytes, nil, nil)
}

func collectOrchidsEventsWithFallback(client *Client, state *requestState, raw string) ([]upstream.SSEMessage, bool) {
	events := make([]upstream.SSEMessage, 0, 2)
	onMessage := func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}
	runtime := newTestRequestRuntime(*state, onMessage)
	rawBytes := []byte(raw)
	handled, shouldBreak := runtime.handleRawMessage(rawBytes, nil, nil)
	if handled {
		return events, shouldBreak
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(rawBytes, &msg); err != nil {
		panic(err)
	}
	return events, runtime.handleDecodedMessage(msg, rawBytes, nil, nil)
}

func newTestRequestRuntime(state requestState, onMessage func(upstream.SSEMessage)) *requestRuntime {
	runtime := &requestRuntime{state: state}
	runtime.writer = NewSSEWriter(&runtime.state, onMessage)
	return runtime
}

func BenchmarkHandleOrchidsTextMessage_Map(b *testing.B) {
	raw := []byte(`{"type":"output_text_delta","text":"hello world"}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = runtime.handleDecodedMessage(msg, raw, nil, nil)
	}
}

func BenchmarkHandleOrchidsTextMessage_Fast(b *testing.B) {
	raw := []byte(`{"type":"output_text_delta","text":"hello world"}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		handled, _ := runtime.handleRawMessage(raw, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle raw text message")
		}
	}
}

func TestHandleOrchidsRawMessageMatchesDecodedModelFinish(t *testing.T) {
	raw := `{"type":"model","event":{"type":"finish","finishReason":"stop"}}`
	client := &Client{}

	legacyState := requestState{textStarted: true}
	wantEvents, _, wantBreak := collectOrchidsEvents(client, &legacyState, raw, false)

	fastState := requestState{textStarted: true}
	gotEvents, gotBreak := collectOrchidsEventsWithFallback(client, &fastState, raw)
	if gotBreak != wantBreak {
		t.Fatalf("shouldBreak=%v want=%v", gotBreak, wantBreak)
	}
	wantJSON, _ := json.Marshal(wantEvents)
	gotJSON, _ := json.Marshal(gotEvents)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("events=%s want=%s", gotJSON, wantJSON)
	}
}

func TestHandleOrchidsRawMessageNormalizesCodeFreeMaxModelFinish(t *testing.T) {
	raw := []byte(`{"type":"model","event":{"data":{"type":"finish","stop_reason":"tool-calls","usage":{"input_tokens":12,"output_tokens":34}}}}`)
	var state requestState
	var events []upstream.SSEMessage
	runtime := newTestRequestRuntime(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})

	handled, shouldBreak := runtime.handleRawMessage(raw, nil, nil)
	if !handled {
		t.Fatal("expected fast path to handle CodeFreeMax finish event")
	}
	if !shouldBreak {
		t.Fatal("expected finish event to break stream loop")
	}
	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if got := events[0].Type; got != "message_delta" {
		t.Fatalf("events[0].Type=%q want message_delta", got)
	}
	if got := events[1].Type; got != "message_stop" {
		t.Fatalf("events[1].Type=%q want message_stop", got)
	}
}

func TestHandleOrchidsRawMessageEmitsDirectFinalFramesForModelFinishInStreamMode(t *testing.T) {
	raw := []byte(`{"type":"model","event":{"data":{"type":"finish","stop_reason":"stop","usage":{"output_tokens":34}}}}`)
	state := requestState{
		stream:      true,
		textStarted: true,
		blockCount:  0,
	}
	var events []upstream.SSEMessage
	runtime := newTestRequestRuntime(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})

	handled, shouldBreak := runtime.handleRawMessage(raw, nil, nil)
	if !handled {
		t.Fatal("expected fast path to handle streamed CodeFreeMax finish event")
	}
	if !shouldBreak {
		t.Fatal("expected streamed finish event to break stream loop")
	}
	// For testing, handleOrchidsRawMessage translates this state to multiple direct frames
	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if got := events[0].Type; got != "message_delta" {
		t.Fatalf("events[0].Type=%q want message_delta", got)
	}
	if got := events[1].Type; got != "message_stop" {
		t.Fatalf("events[1].Type=%q want message_stop", got)
	}
}

func TestHandleOrchidsRawMessageNormalizesCodeFreeMaxToolInputStart(t *testing.T) {
	raw := []byte(`{"type":"model","event":{"data":{"type":"tool-input-start","id":"toolu_read_1","tool_name":"Read"}}}`)
	var state requestState
	var events []upstream.SSEMessage
	runtime := newTestRequestRuntime(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})
	clientTools := []interface{}{
		map[string]interface{}{"name": "read_file", "aliases": []interface{}{"Read"}},
	}

	handled, shouldBreak := runtime.handleRawMessage(raw, nil, clientTools)
	if !handled {
		t.Fatal("expected fast path to handle CodeFreeMax tool-input-start")
	}
	if shouldBreak {
		t.Fatal("did not expect tool-input-start to break stream loop")
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	if got := events[0].Type; got != "content_block_start" {
		t.Fatalf("event type=%v want content_block_start", got)
	}
	if got := events[0].Event["type"]; got != "content_block_start" {
		t.Fatalf("payload type=%v want content_block_start", got)
	}
	contentBlock, _ := events[0].Event["content_block"].(map[string]interface{})
	if got := contentBlock["type"]; got != "tool_use" {
		t.Fatalf("content_block.type=%v want tool_use", got)
	}
	if got := contentBlock["name"]; got != "read_file" {
		t.Fatalf("content_block.name=%v want read_file", got)
	}
	if got := contentBlock["id"]; got != "toolu_read_1" {
		t.Fatalf("content_block.id=%v want toolu_read_1", got)
	}
}

func TestHandleOrchidsMessageNormalizesDecodedCodeFreeMaxToolCall(t *testing.T) {
	var state requestState
	var events []upstream.SSEMessage
	msg := map[string]interface{}{
		"type": "model",
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				"type":      "tool-call",
				"callId":    "call_read_1",
				"tool_name": "Read",
				"input": map[string]interface{}{
					"path": "/tmp/demo.txt",
				},
			},
		},
	}
	clientTools := []interface{}{
		map[string]interface{}{"name": "read_file", "aliases": []interface{}{"Read"}},
	}
	runtime := newTestRequestRuntime(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})

	shouldBreak := runtime.handleDecodedMessage(msg, nil, nil, clientTools)
	if shouldBreak {
		t.Fatal("did not expect tool-call event to break stream loop")
	}
	// For handled tool calls via MapToolNameToClient which we now emulate correctly
	// The decoded message event translates to equivalent direct SSE frames.
	// We expect content_block_start, content_block_delta, content_block_stop.
	if len(events) != 3 {
		t.Fatalf("len(events)=%d want 3 events=%#v", len(events), events)
	}
	if got := events[0].Type; got != "content_block_start" {
		t.Fatalf("events[0].Type=%q want content_block_start", got)
	}
	block, _ := events[0].Event["content_block"].(map[string]interface{})
	if got := block["type"]; got != "tool_use" {
		t.Fatalf("content_block.type=%v want tool_use", got)
	}
	if got := block["name"]; got != "read_file" {
		t.Fatalf("content_block.name=%v want read_file", got)
	}
	if got := block["id"]; got != "call_read_1" {
		t.Fatalf("content_block.id=%v want call_read_1", got)
	}
	if got := events[1].Type; got != "content_block_delta" {
		t.Fatalf("events[1].Type=%q want content_block_delta", got)
	}
	delta, _ := events[1].Event["delta"].(map[string]interface{})
	if got := delta["type"]; got != "input_json_delta" {
		t.Fatalf("delta.type=%v want input_json_delta", got)
	}
	partialJSON, _ := delta["partial_json"].(string)
	if !strings.Contains(partialJSON, `"file_path":"/tmp/demo.txt"`) {
		t.Fatalf("partial_json=%q want normalized file_path", partialJSON)
	}
	if got := events[2].Type; got != "content_block_stop" {
		t.Fatalf("events[2].Type=%q want content_block_stop", got)
	}
}

func TestHandleOrchidsMessageDoesNotInventToolCallID(t *testing.T) {
	var state requestState
	var events []upstream.SSEMessage
	msg := map[string]interface{}{
		"type": "model",
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				"type":      "tool-call",
				"tool_name": "Read",
				"input": map[string]interface{}{
					"path": "/tmp/demo.txt",
				},
			},
		},
	}
	clientTools := []interface{}{
		map[string]interface{}{"name": "read_file"},
	}
	runtime := newTestRequestRuntime(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})

	shouldBreak := runtime.handleDecodedMessage(msg, nil, nil, clientTools)
	if shouldBreak {
		t.Fatal("did not expect tool-call without id to break stream loop")
	}
	if len(events) != 0 {
		t.Fatalf("expected tool-call without id to be dropped, got %d events", len(events))
	}
}

func TestHandleOrchidsRawMessageMatchesDecodedError(t *testing.T) {
	raw := `{"type":"error","data":{"code":"bad_request","message":"boom"}}`
	client := &Client{}

	var legacyState requestState
	wantEvents, _, wantBreak := collectOrchidsEvents(client, &legacyState, raw, false)

	var fastState requestState
	gotEvents, handled, gotBreak := collectOrchidsEvents(client, &fastState, raw, true)
	if !handled {
		t.Fatal("expected fast path to handle error")
	}
	if gotBreak != wantBreak {
		t.Fatalf("shouldBreak=%v want=%v", gotBreak, wantBreak)
	}
	wantJSON, _ := json.Marshal(wantEvents)
	gotJSON, _ := json.Marshal(gotEvents)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("events=%s want=%s", gotJSON, wantJSON)
	}
}

func BenchmarkHandleOrchidsResponseDone_Map(b *testing.B) {
	raw := []byte(`{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","id":"toolu_write_1","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = runtime.handleDecodedMessage(msg, raw, nil, nil)
	}
}

func BenchmarkHandleOrchidsResponseDone_Fast(b *testing.B) {
	raw := []byte(`{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","id":"toolu_write_1","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		handled, _ := runtime.handleRawMessage(raw, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle response.done")
		}
	}
}

func BenchmarkHandleOrchidsModelEvent_Map(b *testing.B) {
	raw := []byte(`{"type":"model","event":{"type":"text-delta","delta":"hello world"}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = runtime.handleDecodedMessage(msg, raw, nil, nil)
	}
}

func BenchmarkHandleOrchidsModelEvent_Fast(b *testing.B) {
	raw := []byte(`{"type":"model","event":{"type":"text-delta","delta":"hello world"}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		handled, _ := runtime.handleRawMessage(raw, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle model event")
		}
	}
}

func BenchmarkHandleOrchidsErrorEvent_Map(b *testing.B) {
	raw := []byte(`{"type":"error","data":{"code":"bad_request","message":"boom"}}`)
	onMessage := func(upstream.SSEMessage) {}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = runtime.handleDecodedMessage(msg, raw, nil, nil)
	}
}

func BenchmarkHandleOrchidsErrorEvent_Fast(b *testing.B) {
	raw := []byte(`{"type":"error","data":{"code":"bad_request","message":"boom"}}`)
	onMessage := func(upstream.SSEMessage) {}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		handled, _ := runtime.handleRawMessage(raw, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle error event")
		}
	}
}

func BenchmarkHandleOrchidsHTTPTextLine_BytesFlow(b *testing.B) {
	line := []byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello world\"}\n")
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		runtime := newTestRequestRuntime(requestState{}, onMessage)
		raw, ok := orchidsSSEDataPayloadBytes(line)
		if !ok {
			b.Fatal("expected payload")
		}
		handled, _ := runtime.handleRawMessage(raw, nil, nil)
		if !handled {
			b.Fatal("expected bytes flow to handle line")
		}
	}
}
