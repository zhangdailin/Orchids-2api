package adapter

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/goccy/go-json"
)

func TestBuildOpenAIChunk(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		data   []byte
		wantOK bool
		want   openAIChunk
	}{
		{
			name:   "message start",
			event:  "message_start",
			data:   []byte("{\"message\":{\"model\":\"claude-3-7-sonnet\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "claude-3-7-sonnet",
				Choices: []openAIChoice{{Index: 0, Delta: openAIDelta{Role: stringPtr("assistant")}}},
			},
		},
		{
			name:   "content block start tool use",
			event:  "content_block_start",
			data:   []byte("{\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_123\",\"name\":\"Write\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{
					Index: 0,
					Delta: openAIDelta{ToolCalls: []openAIToolCall{{
						Index: 0,
						ID:    "tool_123",
						Type:  "function",
						Function: openAIFunction{
							Name:      stringPtr("Write"),
							Arguments: "",
						},
					}}},
				}},
			},
		},
		{
			name:   "content block start text",
			event:  "content_block_start",
			data:   []byte("{\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{Index: 0, Delta: openAIDelta{Content: stringPtr("hello")}}},
			},
		},
		{
			name:   "content block delta text",
			event:  "content_block_delta",
			data:   []byte("{\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{Index: 0, Delta: openAIDelta{Content: stringPtr("hello")}}},
			},
		},
		{
			name:   "content block delta input json",
			event:  "content_block_delta",
			data:   []byte("{\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{
					Index: 0,
					Delta: openAIDelta{ToolCalls: []openAIToolCall{{
						Index:    0,
						Function: openAIFunction{Arguments: "{\"a\":1}"},
					}}},
				}},
			},
		},
		{
			name:   "content block delta thinking",
			event:  "content_block_delta",
			data:   []byte("{\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{Index: 0, Delta: openAIDelta{ReasoningContent: stringPtr("step by step")}}},
			},
		},
		{
			name:   "message delta stop reason",
			event:  "message_delta",
			data:   []byte("{\"delta\":{\"stop_reason\":\"tool_use\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{Index: 0, FinishReason: stringPtr("tool_calls")}},
			},
		},
		{
			name:   "message stop",
			event:  "message_stop",
			data:   []byte("{\"type\":\"message_stop\"}"),
			wantOK: false,
		},
		{
			name:   "message delta end_turn maps to stop",
			event:  "message_delta",
			data:   []byte("{\"delta\":{\"stop_reason\":\"end_turn\"}}"),
			wantOK: true,
			want: openAIChunk{
				ID:      "msg_1",
				Object:  "chat.completion.chunk",
				Created: 123,
				Model:   "",
				Choices: []openAIChoice{{Index: 0, FinishReason: stringPtr("stop")}},
			},
		},
		{
			name:   "content block stop ignored",
			event:  "content_block_stop",
			data:   []byte("{\"type\":\"content_block_stop\"}"),
			wantOK: false,
		},
		{
			name:   "unknown ignored",
			event:  "unknown",
			data:   []byte("{\"type\":\"unknown\"}"),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, ok := BuildOpenAIChunk("msg_1", 123, tt.event, tt.data)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v want=%v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			var got openAIChunk
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got=%#v want=%#v", got, tt.want)
			}
		})
	}
}

func TestBuildOpenAIChunkFastMatchesSlow(t *testing.T) {
	tests := []struct {
		name  string
		event string
		data  []byte
	}{
		{
			name:  "message start",
			event: "message_start",
			data:  []byte("{\"message\":{\"model\":\"claude-3-7-sonnet\"}}"),
		},
		{
			name:  "tool start",
			event: "content_block_start",
			data:  []byte("{\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_123\",\"name\":\"Write\"}}"),
		},
		{
			name:  "text start",
			event: "content_block_start",
			data:  []byte("{\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}"),
		},
		{
			name:  "text delta",
			event: "content_block_delta",
			data:  []byte("{\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}"),
		},
		{
			name:  "input json delta",
			event: "content_block_delta",
			data:  []byte("{\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}"),
		},
		{
			name:  "thinking delta",
			event: "content_block_delta",
			data:  []byte("{\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}"),
		},
		{
			name:  "message delta",
			event: "message_delta",
			data:  []byte("{\"delta\":{\"stop_reason\":\"tool_use\"}}"),
		},
		{
			name:  "message stop",
			event: "message_stop",
			data:  []byte("{\"type\":\"message_stop\"}"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fastRaw, fastOK := buildOpenAIChunkFast("msg_1", 123, tt.event, tt.data)
			slowRaw, slowOK := buildOpenAIChunkSlow("msg_1", 123, tt.event, tt.data)
			if fastOK != slowOK {
				t.Fatalf("fastOK=%v slowOK=%v", fastOK, slowOK)
			}
			if !fastOK {
				return
			}

			var fastChunk openAIChunk
			if err := json.Unmarshal(fastRaw, &fastChunk); err != nil {
				t.Fatalf("unmarshal fast output: %v", err)
			}
			var slowChunk openAIChunk
			if err := json.Unmarshal(slowRaw, &slowChunk); err != nil {
				t.Fatalf("unmarshal slow output: %v", err)
			}
			if !reflect.DeepEqual(fastChunk, slowChunk) {
				t.Fatalf("fast=%#v slow=%#v", fastChunk, slowChunk)
			}
		})
	}
}

func TestAppendOpenAIChunkMatchesBuild(t *testing.T) {
	tests := []struct {
		name  string
		event string
		data  []byte
	}{
		{
			name:  "message start",
			event: "message_start",
			data:  []byte("{\"message\":{\"model\":\"claude-3-7-sonnet\"}}"),
		},
		{
			name:  "tool start",
			event: "content_block_start",
			data:  []byte("{\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_123\",\"name\":\"Write\"}}"),
		},
		{
			name:  "text delta",
			event: "content_block_delta",
			data:  []byte("{\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}"),
		},
		{
			name:  "message delta",
			event: "message_delta",
			data:  []byte("{\"delta\":{\"stop_reason\":\"tool_use\"}}"),
		},
	}

	buf := make([]byte, 0, 512)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, wantOK := BuildOpenAIChunk("msg_1", 123, tt.event, tt.data)
			got, gotOK := AppendOpenAIChunk(buf[:0], "msg_1", 123, tt.event, tt.data)
			if gotOK != wantOK {
				t.Fatalf("ok=%v want=%v", gotOK, wantOK)
			}
			if !gotOK {
				return
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got=%s want=%s", got, want)
			}
			buf = got[:0]
		})
	}
}

func BenchmarkBuildOpenAIChunk_MessageStart(b *testing.B) {
	data := []byte("{\"message\":{\"model\":\"claude-3-7-sonnet\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "message_start", data)
	}
}

func BenchmarkBuildOpenAIChunk_ContentBlockDeltaText(b *testing.B) {
	data := []byte("{\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "content_block_delta", data)
	}
}

func BenchmarkBuildOpenAIChunk_ContentBlockDeltaThinking(b *testing.B) {
	data := []byte("{\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"step by step\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "content_block_delta", data)
	}
}

func BenchmarkBuildOpenAIChunk_ContentBlockDeltaInputJSON(b *testing.B) {
	data := []byte("{\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1,\\\"b\\\":2}\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "content_block_delta", data)
	}
}

func BenchmarkBuildOpenAIChunk_ContentBlockStartToolUse(b *testing.B) {
	data := []byte("{\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_123\",\"name\":\"Write\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "content_block_start", data)
	}
}

func BenchmarkBuildOpenAIChunk_MessageDelta(b *testing.B) {
	data := []byte("{\"delta\":{\"stop_reason\":\"tool_use\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = BuildOpenAIChunk("msg_1", 123, "message_delta", data)
	}
}

func BenchmarkAppendOpenAIChunk_ContentBlockDeltaText_ReusedBuffer(b *testing.B) {
	data := []byte("{\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world\"}}")
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok := AppendOpenAIChunk(buf[:0], "msg_1", 123, "content_block_delta", data)
		if !ok {
			b.Fatal("AppendOpenAIChunk returned false")
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendOpenAIChunk_ContentBlockStartToolUse_ReusedBuffer(b *testing.B) {
	data := []byte("{\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_123\",\"name\":\"Write\"}}")
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok := AppendOpenAIChunk(buf[:0], "msg_1", 123, "content_block_start", data)
		if !ok {
			b.Fatal("AppendOpenAIChunk returned false")
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendOpenAIChunk_MessageDelta_ReusedBuffer(b *testing.B) {
	data := []byte("{\"delta\":{\"stop_reason\":\"tool_use\"}}")
	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok := AppendOpenAIChunk(buf[:0], "msg_1", 123, "message_delta", data)
		if !ok {
			b.Fatal("AppendOpenAIChunk returned false")
		}
		buf = raw[:0]
	}
}
