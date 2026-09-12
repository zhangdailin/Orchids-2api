package handler

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/warp"
)

type mockUpstreamEdge struct {
	events []upstream.SSEMessage
}

type errorUpstreamEdge struct {
	err   error
	calls int
}

type refundingErrorUpstreamEdge struct {
	err             error
	refundErr       error
	calls           int
	refundIDs       []string
	conversationIDs []string
}

func (m *mockUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func (m *errorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.calls++
	return m.err
}

func (m *refundingErrorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.calls++
	return m.err
}

func (m *refundingErrorUpstreamEdge) RefundCredits(ctx context.Context, conversationID, requestID string) error {
	m.conversationIDs = append(m.conversationIDs, conversationID)
	m.refundIDs = append(m.refundIDs, requestID)
	return m.refundErr
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
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(b))
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
	upstreamClient := &refundingErrorUpstreamEdge{err: warp.AttachRequestMetadata(errors.New("dial tcp: connection reset by peer"), "warp-conversation-1", "warp-request-1")}
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

	if len(upstreamClient.refundIDs) != 1 || upstreamClient.refundIDs[0] != "warp-request-1" {
		t.Fatalf("refund request IDs=%#v want warp-request-1", upstreamClient.refundIDs)
	}
	if len(upstreamClient.conversationIDs) != 1 || upstreamClient.conversationIDs[0] != "warp-conversation-1" {
		t.Fatalf("refund conversation IDs=%#v want warp-conversation-1", upstreamClient.conversationIDs)
	}
}

func TestHandleMessages_WarpStartedRequestDoesNotRetryWithoutConfirmedRefund(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 2, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	upstreamClient := &refundingErrorUpstreamEdge{
		err:       warp.AttachRequestMetadata(errors.New("dial tcp: connection reset by peer"), "warp-conversation-1", "warp-request-1"),
		refundErr: errors.New("refund not confirmed"),
	}
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

	if upstreamClient.calls != 1 {
		t.Fatalf("upstream calls=%d want 1; potentially billed request must not retry without confirmed refund", upstreamClient.calls)
	}
}

func TestHandleMessages_PuterStreamQuotaRetrySkipsRetryMarkerAndCoolsDownFailedAccount(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	first := &store.Account{
		AccountType: "puter",
		Enabled:     true,
		Weight:      1,
	}
	if err := s.CreateAccount(context.Background(), first); err != nil {
		t.Fatalf("CreateAccount(first) error = %v", err)
	}
	second := &store.Account{
		AccountType: "puter",
		Enabled:     true,
		Weight:      1,
	}
	if err := s.CreateAccount(context.Background(), second); err != nil {
		t.Fatalf("CreateAccount(second) error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)

	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 1, RetryDelay: 0, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, lb)
	h.connTracker = newSpyConnTracker(map[int64]int64{
		first.ID:  0,
		second.ID: 1,
	})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		if acc.ID == first.ID {
			return &errorUpstreamEdge{err: errors.New("puter API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.")}
		}
		return &mockUpstreamEdge{events: []upstream.SSEMessage{
			{Type: "model", Event: map[string]any{"type": "text-start"}},
			{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "quota-ok"}},
			{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
		}}
	})

	payload := map[string]any{
		"model":    "claude-opus-5",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   true,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "quota-ok") {
		t.Fatalf("expected successful retry output, got: %s", out)
	}
	if strings.Contains(out, "Retrying request") {
		t.Fatalf("did not expect retry marker in streamed assistant output, got: %s", out)
	}

	storedFirst, err := s.GetAccount(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("GetAccount(first) error = %v", err)
	}
	if storedFirst.StatusCode != "402" {
		t.Fatalf("expected first account to be cooled down as 402, got %q", storedFirst.StatusCode)
	}
}

func TestHandleMessages_Dedup_DoesNotSuppressInterruptedRetry(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payload := map[string]any{
		"model": "claude-3-5-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "帮我用python写一个计算器"}}},
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "(no content)"}}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "[Request interrupted by user]\n"},
				{"type": "text", "text": "帮我用python写一个计算器"},
			}},
		},
		"system": []any{},
		"stream": false,
	}
	b, _ := json.Marshal(payload)

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected first request 200, got %d", rec1.Code)
	}
	if strings.Contains(rec1.Body.String(), "duplicate_request") || !strings.Contains(rec1.Body.String(), "ok") {
		t.Fatalf("expected first request to complete normally, got: %s", rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected second request 200, got %d", rec2.Code)
	}
	if strings.Contains(rec2.Body.String(), "duplicate_request") {
		t.Fatalf("expected interrupted retry to bypass dedup, got: %s", rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "ok") {
		t.Fatalf("expected second request to complete normally, got: %s", rec2.Body.String())
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
	req1 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(bodyA))
	h.HandleMessages(rec1, req1)
	if rec1.Code != 200 {
		t.Fatalf("expected first request 200, got %d", rec1.Code)
	}
	if !strings.Contains(rec1.Body.String(), "ok") {
		t.Fatalf("expected first request to complete normally, got: %s", rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(bodyB))
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
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_1", WarpToolBinding{ConversationID: "warp_conv_tool_1", ToolType: "read_files"})

	payload := map[string]any{
		"model":           "claude-3-5-sonnet",
		"conversation_id": "test-conversation",
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
	h.sessionStore.SetWarpToolBinding(context.Background(), "test-conversation", "tool_1", WarpToolBinding{ConversationID: "warp_conv_tool_1", ToolType: "read_files"})

	payload := map[string]any{
		"model":           "claude-3-5-sonnet",
		"conversation_id": "test-conversation",
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

func TestHandleMessages_NonRetryableClientErrorReturnsExplicitMessage(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 3, RetryDelay: 0, ContextMaxTokens: 1024, ContextSummaryMaxTokens: 256, ContextKeepTurns: 2}
	h := NewWithLoadBalancer(cfg, nil)
	upstreamClient := &errorUpstreamEdge{err: errors.New("puter API error: message=Model not found, please try another model")}
	h.client = upstreamClient

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/puter/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamClient.calls != 1 {
		t.Fatalf("expected exactly one upstream call, got %d", upstreamClient.calls)
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Request failed: puter API error: message=Model not found") {
		t.Fatalf("expected explicit upstream error, got: %s", out)
	}
	if strings.Contains(out, "No output was presented to the user") {
		t.Fatalf("did not expect generic empty fallback, got: %s", out)
	}
	if strings.Contains(out, "retries exhausted") {
		t.Fatalf("did not expect retry exhausted wrapper for non-retriable client error, got: %s", out)
	}
}
