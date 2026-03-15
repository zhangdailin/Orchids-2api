package handler

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type mockUpstreamEdge struct {
	events []upstream.SSEMessage
}

type errorUpstreamEdge struct {
	err error
}

type refundingErrorUpstreamEdge struct {
	err           error
	refundReasons []string
}

type directErrorUpstreamEdge struct {
	err             error
	calls           int
	sawNilOnMessage bool
}

type blockingUpstreamEdge struct {
	events    []upstream.SSEMessage
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
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

func (m *errorUpstreamEdge) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return m.err
}

func (m *errorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return m.err
}

func (m *refundingErrorUpstreamEdge) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return m.err
}

func (m *refundingErrorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return m.err
}

func (m *refundingErrorUpstreamEdge) RefundCredits(ctx context.Context, reason string) error {
	m.refundReasons = append(m.refundReasons, reason)
	return nil
}

func (m *directErrorUpstreamEdge) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return m.err
}

func (m *directErrorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.calls++
	m.sawNilOnMessage = onMessage == nil
	if req.DirectSSE != nil {
		req.DirectSSE.ObserveStopReason("error")
		req.DirectSSE.WriteDirectSSE("error", []byte(`{"type":"error","error":{"type":"error","code":"bad_request","message":"boom"}}`), true)
		req.DirectSSE.FinishDirectSSE("error")
	}
	return m.err
}

func (m *directErrorUpstreamEdge) OwnsFinalSSELifecycle() bool {
	return true
}

func (m *blockingUpstreamEdge) SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	return nil
}

func (m *blockingUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
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

func TestHandleMessages_WarpErrorTriggersRefund(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	upstreamClient := &refundingErrorUpstreamEdge{err: errors.New("dial tcp: connection reset by peer")}
	h.client = upstreamClient

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	b, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec, req)

	if len(upstreamClient.refundReasons) != 1 {
		t.Fatalf("expected exactly one refund call, got %#v", upstreamClient.refundReasons)
	}
	if upstreamClient.refundReasons[0] != "network_error" {
		t.Fatalf("refund reason=%q want network_error", upstreamClient.refundReasons[0])
	}
}

func TestHandleMessages_OrchidsDirectErrorDoesNotRetryAfterFinalSSE(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 2, RetryDelay: 0, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	upstreamClient := &directErrorUpstreamEdge{err: errors.New("dial tcp: connection reset by peer")}
	h.client = upstreamClient

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

	if upstreamClient.calls != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", upstreamClient.calls)
	}
	if !upstreamClient.sawNilOnMessage {
		t.Fatal("expected Orchids direct SSE path to bypass onMessage callback")
	}
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if count := strings.Count(out, "event: error"); count != 1 {
		t.Fatalf("expected exactly one error event, got %d: %s", count, out)
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("expected direct error payload, got: %s", out)
	}
	if strings.Contains(out, "Retrying request") {
		t.Fatalf("did not expect retry marker after direct final error, got: %s", out)
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

func TestHandleMessages_Dedup_SemanticBodyDriftWhileInFlight(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	blocking := &blockingUpstreamEdge{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		events: []upstream.SSEMessage{
			{Type: "model", Event: map[string]any{"type": "text-start"}},
			{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
			{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
		},
	}
	h.client = blocking

	payloadA := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"system":   []any{},
		"stream":   false,
	}
	payloadB := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"system": []any{
			map[string]any{"type": "text", "text": "different context wrapper"},
		},
		"stream": false,
	}
	bodyA, _ := json.Marshal(payloadA)
	bodyB, _ := json.Marshal(payloadB)

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(bodyA))
	done1 := make(chan struct{})
	go func() {
		h.HandleMessages(rec1, req1)
		close(done1)
	}()

	<-blocking.entered

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(bodyB))
	h.HandleMessages(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected 200, got %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "duplicate_request") {
		t.Fatalf("expected semantic duplicate suppression, got: %s", rec2.Body.String())
	}

	close(blocking.release)
	<-done1
	if rec1.Code != 200 {
		t.Fatalf("expected first request 200, got %d", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "ok") {
		t.Fatalf("expected first request to complete normally, got: %s", rec1.Body.String())
	}
}

func TestHandleMessages_Dedup_DoesNotSuppressToolResultFollowup(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payloadWithToolResult := func(content string) map[string]any {
		return map[string]any{
			"model": "claude-3-5-sonnet",
			"messages": []map[string]any{
				{"role": "user", "content": "帮我优化这个项目"},
				{"role": "assistant", "content": []map[string]any{
					{
						"type":  "tool_use",
						"id":    "tool_1",
						"name":  "Read",
						"input": map[string]any{"file_path": "/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"},
					},
				}},
				{"role": "user", "content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": "tool_1",
						"content":     content,
					},
				}},
			},
			"system": []any{},
			"stream": false,
		}
	}

	bodyA, _ := json.Marshal(payloadWithToolResult("file one"))
	bodyB, _ := json.Marshal(payloadWithToolResult("file two"))

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(bodyA))
	h.HandleMessages(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected first request 200, got %d", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "ok") {
		t.Fatalf("expected first request to complete normally, got: %s", rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/orchids/v1/messages", bytes.NewReader(bodyB))
	h.HandleMessages(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected second request 200, got %d", rec2.Code)
	}
	if strings.Contains(rec2.Body.String(), "duplicate_request") {
		t.Fatalf("expected tool_result follow-up to bypass semantic dedup, got: %s", rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "ok") {
		t.Fatalf("expected second request to complete normally, got: %s", rec2.Body.String())
	}
}

func TestHandleMessages_ToolResultFollowup_DoesNotInjectLocalFallbackText(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "Let me first understand the project structure and code."}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payload := map[string]any{
		"model": "claude-3-5-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "这个项目使用了哪些技术架构"},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "/Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "tool_1",
					"content":     "import json\nimport os\nALERTS_FILE='alerts.json'\ndef load_json(path):\n    return json.load(open(path))",
				},
				{
					"type": "text",
					"text": "请直接回答",
				},
			}},
		},
		"system": []any{},
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Let me first understand the project structure and code.") {
		t.Fatalf("expected upstream text to be preserved, got: %s", out)
	}
	for _, unwanted := range []string{
		"Python",
		"JSON",
		"基于当前已读取内容",
		"当前只拿到目录概览",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("did not expect local fallback text %q in %s", unwanted, out)
		}
	}
}

func TestHandleMessages_WarpCanceledFollowup_DoesNotEmitGenericEmptyFallback(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &errorUpstreamEdge{err: context.Canceled}

	payload := map[string]any{
		"model": "claude-3-5-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我优化这个项目"},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "tool_1",
					"content":     "1->import os",
				},
			}},
		},
		"system": []any{},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	out := rec.Body.String()
	if strings.Contains(out, "No output was presented to the user") {
		t.Fatalf("did not expect generic empty fallback after canceled upstream, got: %s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected stream to terminate cleanly, got: %s", out)
	}
}
