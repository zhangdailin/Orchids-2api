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
	rawBytes := []byte(raw)
	if fast {
		handled, shouldBreak := client.handleOrchidsRawMessage(rawBytes, state, onMessage, nil, nil)
		return events, handled, shouldBreak
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(rawBytes, &msg); err != nil {
		panic(err)
	}
	return events, true, client.handleOrchidsMessage(msg, rawBytes, state, onMessage, nil, nil)
}

func collectOrchidsEventsWithFallback(client *Client, state *requestState, raw string) ([]upstream.SSEMessage, bool) {
	events := make([]upstream.SSEMessage, 0, 2)
	onMessage := func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}
	rawBytes := []byte(raw)
	handled, shouldBreak := client.handleOrchidsRawMessage(rawBytes, state, onMessage, nil, nil)
	if handled {
		return events, shouldBreak
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(rawBytes, &msg); err != nil {
		panic(err)
	}
	return events, client.handleOrchidsMessage(msg, rawBytes, state, onMessage, nil, nil)
}

func TestHandleOrchidsRawMessageMatchesDecodedTextEvents(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "output_text_delta text", raw: `{"type":"output_text_delta","text":"hello"}`},
		{name: "response_chunk object", raw: `{"type":"coding_agent.response.chunk","chunk":{"content":"hello"}}`},
		{name: "reasoning chunk", raw: `{"type":"coding_agent.reasoning.chunk","data":{"text":"think"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{}
			var legacyState requestState
			wantEvents, _, wantBreak := collectOrchidsEvents(client, &legacyState, tt.raw, false)

			var fastState requestState
			gotEvents, handled, gotBreak := collectOrchidsEvents(client, &fastState, tt.raw, true)
			if !handled {
				t.Fatal("expected fast path to handle message")
			}
			if gotBreak != wantBreak {
				t.Fatalf("shouldBreak=%v want=%v", gotBreak, wantBreak)
			}
			wantJSON, _ := json.Marshal(wantEvents)
			gotJSON, _ := json.Marshal(gotEvents)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("events=%s want=%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestHandleOrchidsRawMessageMatchesDecodedCrossChannelDedup(t *testing.T) {
	client := &Client{}
	var legacyState requestState
	_, _, _ = collectOrchidsEvents(client, &legacyState, `{"type":"output_text_delta","text":"hello"}`, false)
	legacyEvents, _, _ := collectOrchidsEvents(client, &legacyState, `{"type":"coding_agent.response.chunk","chunk":"hello"}`, false)

	var fastState requestState
	_, _, _ = collectOrchidsEvents(client, &fastState, `{"type":"output_text_delta","text":"hello"}`, true)
	fastEvents, handled, _ := collectOrchidsEvents(client, &fastState, `{"type":"coding_agent.response.chunk","chunk":"hello"}`, true)
	if !handled {
		t.Fatal("expected fast path to handle response chunk")
	}
	if len(fastEvents) != len(legacyEvents) {
		t.Fatalf("event count=%d want=%d", len(fastEvents), len(legacyEvents))
	}
}

func BenchmarkHandleOrchidsTextMessage_Map(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"output_text_delta","text":"hello world"}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = client.handleOrchidsMessage(msg, raw, &state, onMessage, nil, nil)
	}
}

func BenchmarkHandleOrchidsTextMessage_Fast(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"output_text_delta","text":"hello world"}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		handled, _ := client.handleOrchidsRawMessage(raw, &state, onMessage, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle raw text message")
		}
	}
}

func TestHandleOrchidsRawMessageMatchesDecodedResponseDone(t *testing.T) {
	raw := `{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`
	client := &Client{}

	var legacyState requestState
	wantEvents, _, wantBreak := collectOrchidsEvents(client, &legacyState, raw, false)

	var fastState requestState
	gotEvents, handled, gotBreak := collectOrchidsEvents(client, &fastState, raw, true)
	if !handled {
		t.Fatal("expected fast path to handle response.done")
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

func TestHandleOrchidsResponseDoneEmitsCodeFreeMaxToolUseBlocks(t *testing.T) {
	raw := `{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`
	client := &Client{}
	var state requestState

	events, handled, shouldBreak := collectOrchidsEvents(client, &state, raw, true)
	if !handled {
		t.Fatal("expected fast path to handle response.done")
	}
	if !shouldBreak {
		t.Fatal("expected response.done to end the stream")
	}
	if len(events) < 5 {
		t.Fatalf("len(events)=%d want at least 5", len(events))
	}

	if got := events[0].Event["type"]; got != "tokens-used" {
		t.Fatalf("events[0].type=%v want tokens-used", got)
	}
	if got := events[1].Event["type"]; got != "content_block_start" {
		t.Fatalf("events[1].type=%v want content_block_start", got)
	}
	contentBlock, _ := events[1].Event["content_block"].(map[string]interface{})
	if got := contentBlock["type"]; got != "tool_use" {
		t.Fatalf("events[1].content_block.type=%v want tool_use", got)
	}
	if got := events[2].Event["type"]; got != "content_block_delta" {
		t.Fatalf("events[2].type=%v want content_block_delta", got)
	}
	delta, _ := events[2].Event["delta"].(map[string]interface{})
	if got := delta["type"]; got != "input_json_delta" {
		t.Fatalf("events[2].delta.type=%v want input_json_delta", got)
	}
	if got := events[3].Event["type"]; got != "content_block_stop" {
		t.Fatalf("events[3].type=%v want content_block_stop", got)
	}
	if got := events[4].Event["type"]; got != "finish" {
		t.Fatalf("events[4].type=%v want finish", got)
	}
}

func TestHandleOrchidsOutputTextDeltaEmitsFinalContentBlockEvents(t *testing.T) {
	raw := `{"type":"output_text_delta","text":"hello"}`
	client := &Client{}
	var state requestState

	events, handled, shouldBreak := collectOrchidsEvents(client, &state, raw, true)
	if !handled {
		t.Fatal("expected fast path to handle output_text_delta")
	}
	if shouldBreak {
		t.Fatal("did not expect output_text_delta to break stream loop")
	}
	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "content_block_start" {
		t.Fatalf("events[0].Type=%q want content_block_start", events[0].Type)
	}
	if got := events[0].Event["type"]; got != "content_block_start" {
		t.Fatalf("events[0].type=%v want content_block_start", got)
	}
	if events[1].Type != "content_block_delta" {
		t.Fatalf("events[1].Type=%q want content_block_delta", events[1].Type)
	}
	delta, _ := events[1].Event["delta"].(map[string]interface{})
	if got := delta["type"]; got != "text_delta" {
		t.Fatalf("delta.type=%v want text_delta", got)
	}
	if got := delta["text"]; got != "hello" {
		t.Fatalf("delta.text=%v want hello", got)
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

func TestHandleOrchidsRawMessageSuppressesDuplicateModelText(t *testing.T) {
	raw := `{"type":"model","event":{"type":"text-delta","delta":"hello"}}`
	client := &Client{}
	state := requestState{preferCodingAgent: true}
	events, handled, shouldBreak := collectOrchidsEvents(client, &state, raw, true)
	if !handled {
		t.Fatal("expected fast path to suppress duplicate model text")
	}
	if shouldBreak {
		t.Fatal("did not expect break for suppressed model text")
	}
	if len(events) != 0 {
		t.Fatalf("expected no emitted events, got %d", len(events))
	}
}

func TestHandleOrchidsRawMessageNormalizesCodeFreeMaxModelFinish(t *testing.T) {
	raw := []byte(`{"type":"model","event":{"data":{"type":"finish","stop_reason":"tool-calls","usage":{"input_tokens":12,"output_tokens":34}}}}`)
	client := &Client{}
	var state requestState
	var events []upstream.SSEMessage

	handled, shouldBreak := client.handleOrchidsRawMessage(raw, &state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil, nil)
	if !handled {
		t.Fatal("expected fast path to handle CodeFreeMax finish event")
	}
	if !shouldBreak {
		t.Fatal("expected finish event to break stream loop")
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	if got := events[0].Event["type"]; got != "finish" {
		t.Fatalf("event type=%v want finish", got)
	}
	if got := events[0].Event["finishReason"]; got != "tool-calls" {
		t.Fatalf("finishReason=%v want tool-calls", got)
	}
	usage, ok := events[0].Event["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("usage type=%T want map[string]interface{}", events[0].Event["usage"])
	}
	if got := usage["inputTokens"]; got != float64(12) {
		t.Fatalf("inputTokens=%v want 12", got)
	}
	if got := usage["outputTokens"]; got != float64(34) {
		t.Fatalf("outputTokens=%v want 34", got)
	}
}

func TestHandleOrchidsRawMessageNormalizesCodeFreeMaxToolInputStart(t *testing.T) {
	raw := []byte(`{"type":"model","event":{"data":{"type":"tool-input-start","id":"toolu_read_1","tool_name":"Read"}}}`)
	client := &Client{}
	var state requestState
	var events []upstream.SSEMessage
	clientTools := []interface{}{
		map[string]interface{}{"name": "read_file"},
	}

	handled, shouldBreak := client.handleOrchidsRawMessage(raw, &state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil, clientTools)
	if !handled {
		t.Fatal("expected fast path to handle CodeFreeMax tool-input-start")
	}
	if shouldBreak {
		t.Fatal("did not expect tool-input-start to break stream loop")
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	if got := events[0].Event["type"]; got != "tool-input-start" {
		t.Fatalf("event type=%v want tool-input-start", got)
	}
	if got := events[0].Event["toolName"]; got != "read_file" {
		t.Fatalf("toolName=%v want read_file", got)
	}
	if got := events[0].Event["id"]; got != "toolu_read_1" {
		t.Fatalf("id=%v want toolu_read_1", got)
	}
}

func TestHandleOrchidsMessageNormalizesDecodedCodeFreeMaxToolCall(t *testing.T) {
	client := &Client{}
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
		map[string]interface{}{"name": "read_file"},
	}

	shouldBreak := client.handleOrchidsMessage(msg, nil, &state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil, clientTools)
	if shouldBreak {
		t.Fatal("did not expect tool-call event to break stream loop")
	}
	if len(events) != 1 {
		t.Fatalf("len(events)=%d want 1", len(events))
	}
	if got := events[0].Event["toolName"]; got != "read_file" {
		t.Fatalf("toolName=%v want read_file", got)
	}
	input, _ := events[0].Event["input"].(string)
	if !strings.Contains(input, `"file_path":"/tmp/demo.txt"`) {
		t.Fatalf("input=%q want normalized file_path", input)
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

func TestHandleOrchidsRawMessageMatchesDecodedTokensUsed(t *testing.T) {
	raw := `{"type":"coding_agent.tokens_used","data":{"input_tokens":12,"output_tokens":34}}`
	client := &Client{}

	var legacyState requestState
	wantEvents, _, wantBreak := collectOrchidsEvents(client, &legacyState, raw, false)

	var fastState requestState
	gotEvents, handled, gotBreak := collectOrchidsEvents(client, &fastState, raw, true)
	if !handled {
		t.Fatal("expected fast path to handle tokens event")
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
	client := &Client{}
	raw := []byte(`{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = client.handleOrchidsMessage(msg, raw, &state, onMessage, nil, nil)
	}
}

func BenchmarkHandleOrchidsResponseDone_Fast(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"response_done","response":{"usage":{"inputTokens":12,"outputTokens":34},"output":[{"type":"tool_use","name":"Write","input":{"file_path":"/tmp/a.txt","content":"hello"}}]}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		handled, _ := client.handleOrchidsRawMessage(raw, &state, onMessage, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle response.done")
		}
	}
}

func BenchmarkHandleOrchidsModelEvent_Map(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"model","event":{"type":"text-delta","delta":"hello world"}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		state := requestState{preferCodingAgent: true}
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = client.handleOrchidsMessage(msg, raw, &state, onMessage, nil, nil)
	}
}

func BenchmarkHandleOrchidsModelEvent_Fast(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"model","event":{"type":"text-delta","delta":"hello world"}}`)
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		state := requestState{preferCodingAgent: true}
		handled, _ := client.handleOrchidsRawMessage(raw, &state, onMessage, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle model event")
		}
	}
}

func BenchmarkHandleOrchidsErrorEvent_Map(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"error","data":{"code":"bad_request","message":"boom"}}`)
	onMessage := func(upstream.SSEMessage) {}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			b.Fatal(err)
		}
		_ = client.handleOrchidsMessage(msg, raw, &state, onMessage, nil, nil)
	}
}

func BenchmarkHandleOrchidsErrorEvent_Fast(b *testing.B) {
	client := &Client{}
	raw := []byte(`{"type":"error","data":{"code":"bad_request","message":"boom"}}`)
	onMessage := func(upstream.SSEMessage) {}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		handled, _ := client.handleOrchidsRawMessage(raw, &state, onMessage, nil, nil)
		if !handled {
			b.Fatal("expected fast path to handle error event")
		}
	}
}

func BenchmarkHandleOrchidsHTTPTextLine_BytesFlow(b *testing.B) {
	client := &Client{}
	line := []byte("data: {\"type\":\"output_text_delta\",\"text\":\"hello world\"}\n")
	onMessage := func(upstream.SSEMessage) {}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var state requestState
		raw, ok := orchidsSSEDataPayloadBytes(line)
		if !ok {
			b.Fatal("expected payload")
		}
		handled, _ := client.handleOrchidsRawMessage(raw, &state, onMessage, nil, nil)
		if !handled {
			b.Fatal("expected bytes flow to handle line")
		}
	}
}
