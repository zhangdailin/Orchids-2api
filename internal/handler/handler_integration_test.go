package handler

import (
	"bytes"
	"context"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type mockUpstream struct {
	events []upstream.SSEMessage
}

type panicUpstream struct{}

func (m *mockUpstream) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func (m *mockUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func (p *panicUpstream) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	panic("unexpected upstream request")
}

func (p *panicUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	panic("unexpected upstream request")
}

func TestHandleMessages_Orchids_StreamAndJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hello"}},
		{Type: "model", Event: map[string]any{"type": "text-end"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"system":   []any{},
			"stream":   stream,
		}
		b, _ := json.Marshal(payload)
		return b
	}

	// non-stream JSON
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Fatalf("expected json content-type, got %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "\"type\":\"message\"") {
			t.Fatalf("expected message json, got: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "hello") {
			t.Fatalf("expected upstream text in response")
		}
	}

	// stream SSE
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			t.Fatalf("expected sse content-type, got %q", ct)
		}
		out := rec.Body.String()
		if !strings.Contains(out, "event: message_start") {
			t.Fatalf("expected message_start, got: %s", out)
		}
		if !strings.Contains(out, "hello") {
			t.Fatalf("expected text delta in SSE")
		}
		if !strings.Contains(out, "event: message_stop") {
			t.Fatalf("expected message_stop, got: %s", out)
		}
	}
}

func TestHandleMessages_Orchids_DoesNotFilterToolCallsByDeclaredTools(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model.tool-call", Event: map[string]any{
			"toolCallId": "tool_edit_1",
			"toolName":   "Edit",
			"input":      `{"file_path":"/tmp/demo.txt","old_string":"hello","new_string":"world"}`,
		}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "tool_use"}},
	}}

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "Read",
					"parameters": map[string]any{
						"type": "object",
					},
				},
			},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use block, got: %s", out)
	}
	if !strings.Contains(out, `"name":"Edit"`) {
		t.Fatalf("expected Edit tool call to pass through, got: %s", out)
	}
}

func TestHandleMessages_Warp_StreamAndJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "conversation_id", "id": "conv1"}},
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "warp-hi"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"system":   []any{},
			"stream":   stream,
			// include stable conversation_id so handler will store upstream conv id
			"conversation_id": "c1",
		}
		b, _ := json.Marshal(payload)
		return b
	}

	// non-stream JSON
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "warp-hi") {
			t.Fatalf("expected upstream text in response")
		}
	}

	// stream SSE
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		out := rec.Body.String()
		if !strings.Contains(out, "warp-hi") {
			t.Fatalf("expected text delta in SSE")
		}
	}

	// ensure upstream conversation id stored via SessionStore
	convKey := conversationKeyForRequest(httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", nil), ClaudeRequest{ConversationID: "c1"})
	got, _ := h.sessionStore.GetConvID(context.Background(), convKey)
	if got != "conv1" {
		t.Fatalf("expected stored upstream conversation id conv1, got %q", got)
	}
}

func TestHandleMessages_Bolt_OpenAINonStreamJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hello from bolt"}},
		{Type: "model.tool-call", Event: map[string]any{
			"toolCallId": "call_1",
			"toolName":   "Read",
			"input":      `{"file_path":"/tmp/demo.txt"}`,
		}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "tool_use"}},
	}}

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-6",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/bolt/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Fatalf("expected json content-type, got %q", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if got["object"] != "chat.completion" {
		t.Fatalf("expected chat.completion object, got %#v", got["object"])
	}
	choices, ok := got["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected one choice, got %#v", got["choices"])
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatalf("expected choice object, got %#v", choices[0])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("expected tool_calls finish_reason, got %#v", choice["finish_reason"])
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message object, got %#v", choice["message"])
	}
	if message["role"] != "assistant" {
		t.Fatalf("expected assistant role, got %#v", message["role"])
	}
	if message["content"] != "hello from bolt" {
		t.Fatalf("expected text content, got %#v", message["content"])
	}
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("expected one tool call, got %#v", message["tool_calls"])
	}
	toolCall, ok := toolCalls[0].(map[string]any)
	if !ok {
		t.Fatalf("expected tool call object, got %#v", toolCalls[0])
	}
	if toolCall["type"] != "function" {
		t.Fatalf("expected function tool type, got %#v", toolCall["type"])
	}
	function, ok := toolCall["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected function object, got %#v", toolCall["function"])
	}
	if function["name"] != "Read" {
		t.Fatalf("expected Read tool name, got %#v", function["name"])
	}
	if function["arguments"] != `{"file_path":"/tmp/demo.txt"}` {
		t.Fatalf("unexpected tool arguments: %#v", function["arguments"])
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("expected usage object, got %#v", got["usage"])
	}
	if _, ok := usage["prompt_tokens"]; !ok {
		t.Fatalf("expected prompt_tokens in usage, got %#v", usage)
	}
	if _, ok := usage["completion_tokens"]; !ok {
		t.Fatalf("expected completion_tokens in usage, got %#v", usage)
	}
	if _, ok := usage["total_tokens"]; !ok {
		t.Fatalf("expected total_tokens in usage, got %#v", usage)
	}
}

func TestHandleMessages_Puter_StreamAndJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "puter-hi"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model":    "claude-opus-4-5",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"system":   []any{},
			"stream":   stream,
		}
		b, _ := json.Marshal(payload)
		return b
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "puter-hi") {
			t.Fatalf("expected upstream text in response")
		}
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		out := rec.Body.String()
		if !strings.Contains(out, "puter-hi") {
			t.Fatalf("expected text delta in SSE")
		}
	}
}

func TestHandleMessages_SuggestionMode_LocalResponse(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &panicUpstream{}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model": "claude-3-5-sonnet",
			"messages": []map[string]any{
				{"role": "user", "content": "继续处理这个问题"},
				{"role": "assistant", "content": "已经定位完了。如果你要，我下一步可以直接帮你提交修复。"},
				{"role": "user", "content": "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"},
			},
			"system": []any{},
			"stream": stream,
		}
		b, _ := json.Marshal(payload)
		return b
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "\"type\":\"message\"") {
			t.Fatalf("expected message json, got: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "可以") {
			t.Fatalf("expected local suggestion in response, got: %s", rec.Body.String())
		}
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		if rec.Code != 200 {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		out := rec.Body.String()
		if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
			t.Fatalf("expected sse message start/stop, got: %s", out)
		}
		if !strings.Contains(out, "可以") {
			t.Fatalf("expected local suggestion in sse output, got: %s", out)
		}
	}
}
