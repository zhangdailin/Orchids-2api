package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

type relayRecordingClient struct{ requests []upstream.UpstreamRequest }

func (c *relayRecordingClient) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.requests = append(c.requests, req)
	for _, event := range []map[string]interface{}{{"type": "text-start"}, {"type": "text-delta", "delta": "upstream answer"}, {"type": "finish", "finishReason": "stop"}} {
		emit(upstream.SSEMessage{Type: "model", Event: event})
	}
	return nil
}

func TestRelayRepeatedRequestsAlwaysReachUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
		client := &relayRecordingClient{}
		h.client = client
		body, _ := json.Marshal(map[string]interface{}{"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "Explain this example"}}, "stream": stream})
		for _, key := range []string{"", "", "once", "once"} {
			r := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
			r.Header.Set("Idempotency-Key", key)
			r.Header.Set("X-Stainless-Retry-Count", "1")
			w := httptest.NewRecorder()
			before := len(client.requests)
			h.HandleMessages(w, r)
			if w.Code != http.StatusOK || len(client.requests) != before+1 || !strings.Contains(w.Body.String(), "upstream answer") {
				t.Fatalf("valid repeated request suppressed: status=%d calls=%d body=%s", w.Code, len(client.requests), w.Body.String())
			}
		}
	}
}

func TestRelayWorkdirChangePreservesHistory(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	client := &relayRecordingClient{}
	h.client = client
	ctx := context.Background()
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: strings.Repeat("Earlier context must survive. ", 100)}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "Earlier response"}},
		{Role: "user", Content: prompt.MessageContent{Text: "Continue explaining the example"}},
	}
	body, _ := json.Marshal(ClaudeRequest{Model: "claude-3-5-sonnet", ConversationID: "relay-history", Messages: messages})
	r := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", bytes.NewReader(body))
	r.Header.Set("X-Workdir", "D:/new-project")
	var parsed ClaudeRequest
	_ = json.Unmarshal(body, &parsed)
	key := conversationKeyForRequest(r, parsed)
	h.sessionStore.SetWorkdir(ctx, key, "D:/old-project")
	w := httptest.NewRecorder()
	h.HandleMessages(w, r)
	if w.Code != 200 || len(client.requests) != 1 {
		t.Fatalf("status=%d body=%s calls=%d", w.Code, w.Body.String(), len(client.requests))
	}
	got := client.requests[0].Messages
	if len(got) != len(messages) {
		t.Fatalf("history shortened: %v", got)
	}
	for i := range messages {
		if got[i].Role != messages[i].Role || got[i].ExtractText() != messages[i].ExtractText() {
			t.Fatalf("message %d rewritten: %+v", i, got[i])
		}
	}
}

func TestRelayIdempotencyKeyIsCallerScoped(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	client := &relayRecordingClient{}
	h.client = client
	for _, caller := range []string{"caller-a", "caller-b"} {
		r := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Explain this example"}]}`))
		r.Header.Set("X-API-Key", caller)
		r.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		h.HandleMessages(w, r)
		if w.Code != 200 {
			t.Fatalf("cross-caller dedup: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if len(client.requests) != 2 {
		t.Fatalf("calls=%d", len(client.requests))
	}
}

func TestRelayRepeatedTextIsNotFiltered(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		rec := httptest.NewRecorder()
		h := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, streaming, adapter.FormatAnthropic, "")
		text := "Hi! How can I help you today?"
		h.handleMessage(upstream.SSEMessage{Type: "model.text-start", Event: map[string]interface{}{}})
		for i := 0; i < 3; i++ {
			h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": text + text}})
		}
		h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "stop"}})
		got := h.responseText.String()
		if streaming {
			got = rec.Body.String()
		}
		if strings.Count(got, text) != 6 {
			t.Errorf("stream=%v repeated text changed: %q", streaming, got)
		}
		h.release()
	}
}

func TestRelayRepeatedToolCallsHaveIndependentIDs(t *testing.T) {
	for _, format := range []adapter.ResponseFormat{adapter.FormatAnthropic, adapter.FormatOpenAI} {
		for _, streaming := range []bool{false, true} {
			rec := httptest.NewRecorder()
			h := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, streaming, format, "")
			for _, id := range []string{"call_one", "call_two"} {
				h.handleMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{"toolCallId": id, "toolName": "Write", "input": `{"file_path":"repeat.txt","content":"same content"}`}})
			}
			h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "tool_use"}})
			if h.toolCallCount != 2 {
				t.Errorf("format=%v stream=%v calls=%d", format, streaming, h.toolCallCount)
			}
			wire := rec.Body.String()
			if !streaming {
				raw, _ := json.Marshal(h.contentBlocks)
				wire = string(raw)
			}
			for _, id := range []string{"call_one", "call_two"} {
				if !strings.Contains(wire, id) {
					t.Errorf("format=%v stream=%v tool call identity lost: %s; output=%s", format, streaming, id, wire)
				}
			}
			h.release()
		}
	}
}

type relayConcurrentClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *relayConcurrentClient) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.entered <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": "parallel answer"}})
	emit(upstream.SSEMessage{Type: "model.finish", Event: map[string]interface{}{"finishReason": "stop"}})
	return nil
}

func TestRelayIdenticalInflightRequestsBothReachUpstream(t *testing.T) {
	client := &relayConcurrentClient{entered: make(chan struct{}, 2), release: make(chan struct{})}
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = client
	finished := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r := httptest.NewRequest(http.MethodPost, "/puter/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Explain the same example"}]}`))
			r.Header.Set("Idempotency-Key", "same-inflight-key")
			w := httptest.NewRecorder()
			h.HandleMessages(w, r)
			finished <- w
		}()
	}
	entered := 0
	for entered < 2 {
		select {
		case <-client.entered:
			entered++
		case <-time.After(3 * time.Second):
			close(client.release)
			t.Fatal("an identical inflight request was suppressed")
		}
	}
	close(client.release)
	for i := 0; i < 2; i++ {
		select {
		case w := <-finished:
			if w.Code != 200 || !strings.Contains(w.Body.String(), "parallel answer") {
				t.Errorf("status=%d body=%s", w.Code, w.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("parallel request failed to finish")
		}
	}
}
