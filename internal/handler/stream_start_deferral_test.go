package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

// sharedRefusalUpstream always fails the way a queue-throttled upstream does, and
// counts the attempts so a test can tell a single probe from a spent budget.
type sharedRefusalUpstream struct {
	calls int
}

func (m *sharedRefusalUpstream) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, _ func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	return errors.New(`qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":30}"}`)
}

// recoveringRefusalUpstream succeeds after its queue probe so the retry path
// must return an answer without requiring another client-side request.
type recoveringRefusalUpstream struct {
	calls int
}

func (m *recoveringRefusalUpstream) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	if m.calls <= 2 {
		return errors.New(`qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":30}"}`)
	}
	onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "ready"}})
	onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}})
	return nil
}

func TestSharedRefusalRecoversInsideSingleRequest(t *testing.T) {
	cfg := &config.Config{RequestTimeout: 10, MaxRetries: 3, RetryDelay: 1}
	h := NewWithLoadBalancer(cfg, nil)
	stub := &recoveringRefusalUpstream{}
	h.client = stub
	payload := map[string]any{"model": "qwen3.8-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	if stub.calls != 3 || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ready") {
		t.Fatalf("calls=%d status=%d body=%s", stub.calls, rec.Code, rec.Body.String())
	}
}

// TestStreamOpensOnlyWhenThereIsSomethingToSend pins the deferral: a streaming
// response must not commit its status before the upstream has produced anything,
// or a failure can no longer be answered with a status.
func TestStreamOpensOnlyWhenThereIsSomethingToSend(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	if got := rec.buf.String(); got != "" {
		t.Fatalf("nothing had been produced, yet the client already received: %q", got)
	}
	if sh.hasCommitted() {
		t.Fatal("hasCommitted() = true before any output")
	}

	sh.handleMessage(upstream.SSEMessage{
		Type:  "model.text-delta",
		Event: map[string]any{"delta": "hello"},
	})

	out := rec.buf.String()
	startAt := strings.Index(out, "event: message_start")
	textAt := strings.Index(out, `"text":"hello"`)
	if startAt < 0 || textAt < 0 {
		t.Fatalf("expected an opening frame followed by the text, got: %s", out)
	}
	if startAt > textAt {
		t.Fatalf("content arrived before the opening frame: %s", out)
	}
	if strings.Count(out, "event: message_start") != 1 {
		t.Fatalf("expected exactly one opening frame, got: %s", out)
	}
	if !sh.hasCommitted() {
		t.Fatal("hasCommitted() = false after the opening frame was written")
	}
}

// TestKeepAliveOpensTheStreamInsteadOfStayingSilent pins the liveness bound: a
// keep-alive tick means the upstream has been silent for the whole interval, so
// the stream opens and the client is kept alive rather than left with nothing.
func TestKeepAliveOpensTheStreamInsteadOfStayingSilent(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	sh.writeKeepAlive()

	out := rec.buf.String()
	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("expected the keep-alive to open the stream, got: %q", out)
	}
	if !strings.Contains(out, sseKeepAlive) {
		t.Fatalf("expected a keep-alive comment, got: %q", out)
	}
}

// TestTerminalOnlyResponseStillOpensTheStream covers an answer that produced no
// content: the opening frame must still precede the terminal events, or the
// client receives a stop for a message that never started.
func TestTerminalOnlyResponseStillOpensTheStream(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	sh.finishResponse("end_turn")

	out := rec.buf.String()
	startAt := strings.Index(out, "event: message_start")
	deltaAt := strings.Index(out, "event: message_delta")
	stopAt := strings.Index(out, "event: message_stop")
	if startAt < 0 || deltaAt < 0 || stopAt < 0 {
		t.Fatalf("expected opening and terminal frames, got: %s", out)
	}
	if !(startAt < deltaAt && deltaAt < stopAt) {
		t.Fatalf("frames out of order: %s", out)
	}
}

// TestSharedRefusalBeforeOutputUsesRetryWindow confirms that the server keeps
// the same request open through its bounded retry window. If the upstream does
// not recover, it returns an HTTP 429 without opening an SSE stream.
func TestSharedRefusalBeforeOutputUsesRetryWindow(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 3, RetryDelay: 1}
	h := NewWithLoadBalancer(cfg, nil)
	stub := &sharedRefusalUpstream{}
	h.client = stub

	payload := map[string]any{
		"model":    "qwen3.8-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", bytes.NewReader(body))

	h.HandleMessages(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "event: error") || strings.Contains(out, "data:") {
		t.Fatalf("an uncommitted stream must not answer with SSE frames: %s", out)
	}
	// Initial request plus the three configured retry probes.
	if stub.calls != 4 {
		t.Fatalf("upstream attempts = %d, want 4 (initial request plus retry budget)", stub.calls)
	}
}
