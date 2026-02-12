package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type mockUpstreamEdge struct {
	events []upstream.SSEMessage
}

func (m *mockUpstreamEdge) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	// not used
	return nil
}

func (m *mockUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func TestHandleMessages_Stream_NoFinish_StillStops(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hello"}},
		// no finish
	}}

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   true,
	}
	b, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec, req)
	out := rec.Body.String()
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected text delta")
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected forced message_stop when upstream missing finish, got: %s", out)
	}
}

func TestHandleMessages_Dedup_NonStream(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	b, _ := json.Marshal(payload)

	// first request
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected 200, got %d", rec1.Code)
	}

	// second request within dedup window
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	body := rec2.Body.String()
	if !strings.Contains(body, "duplicate_request") {
		t.Fatalf("expected duplicate_request response, got: %s", body)
	}
}

func TestHandleMessages_Dedup_Stream(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   true,
	}
	b, _ := json.Marshal(payload)

	// first request
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected 200, got %d", rec1.Code)
	}

	// second request within dedup window
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	out := rec2.Body.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected minimal sse start/stop for duplicate, got: %s", out)
	}
}
