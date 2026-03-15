package orchids

import (
	"testing"

	"orchids-api/internal/upstream"
)

type directSSERecorder struct {
	writes      []directSSEWrite
	finishCalls []string
}

type directSSEWrite struct {
	event string
	final bool
}

func (r *directSSERecorder) WriteDirectSSE(event string, payload []byte, final bool) {
	r.writes = append(r.writes, directSSEWrite{event: event, final: final})
}

func (r *directSSERecorder) ObserveTextDelta(text string)       {}
func (r *directSSERecorder) ObserveThinkingDelta(text string)   {}
func (r *directSSERecorder) ObserveToolCall(name, input string) {}
func (r *directSSERecorder) ObserveUsage(inputTokens, outputTokens int) {
}
func (r *directSSERecorder) ObserveStopReason(stopReason string) {}
func (r *directSSERecorder) FinishDirectSSE(stopReason string) {
	r.finishCalls = append(r.finishCalls, stopReason)
}

var _ upstream.DirectSSEEmitter = (*directSSERecorder)(nil)

func TestSSEWriterWriteMessageEndIsIdempotentWithoutDirectEmitter(t *testing.T) {
	t.Parallel()

	state := &requestState{
		stream:     true,
		modelName:  "claude-sonnet-4-6",
		blockCount: 8,
	}
	var events []upstream.SSEMessage
	writer := NewSSEWriter(state, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})

	if err := writer.WriteMessageEnd(); err != nil {
		t.Fatalf("first WriteMessageEnd() error = %v", err)
	}
	if err := writer.WriteMessageEnd(); err != nil {
		t.Fatalf("second WriteMessageEnd() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events)=%d want 2", len(events))
	}
	if events[0].Type != "message_delta" {
		t.Fatalf("events[0].Type=%q want message_delta", events[0].Type)
	}
	if events[1].Type != "message_stop" {
		t.Fatalf("events[1].Type=%q want message_stop", events[1].Type)
	}
}

func TestSSEWriterWriteMessageEndIsIdempotentWithDirectEmitter(t *testing.T) {
	t.Parallel()

	recorder := &directSSERecorder{}
	state := &requestState{
		stream:       true,
		modelName:    "claude-sonnet-4-6",
		blockCount:   8,
		directSSE:    recorder,
		finishReason: "end_turn",
	}
	writer := NewSSEWriter(state, nil)
	if writer == nil {
		t.Fatal("expected direct SSE writer without onMessage callback")
	}

	if err := writer.WriteMessageEnd(); err != nil {
		t.Fatalf("first WriteMessageEnd() error = %v", err)
	}
	if err := writer.WriteMessageEnd(); err != nil {
		t.Fatalf("second WriteMessageEnd() error = %v", err)
	}

	if len(recorder.writes) != 2 {
		t.Fatalf("len(writes)=%d want 2", len(recorder.writes))
	}
	if recorder.writes[0].event != "message_delta" {
		t.Fatalf("writes[0].event=%q want message_delta", recorder.writes[0].event)
	}
	if recorder.writes[0].final {
		t.Fatal("message_delta should not be final")
	}
	if recorder.writes[1].event != "message_stop" {
		t.Fatalf("writes[1].event=%q want message_stop", recorder.writes[1].event)
	}
	if !recorder.writes[1].final {
		t.Fatal("message_stop should be final")
	}
	if len(recorder.finishCalls) != 1 {
		t.Fatalf("len(finishCalls)=%d want 1", len(recorder.finishCalls))
	}
	if recorder.finishCalls[0] != "end_turn" {
		t.Fatalf("finishCalls[0]=%q want end_turn", recorder.finishCalls[0])
	}
}

func TestSSEWriterWriteErrorIsIdempotentWithDirectEmitter(t *testing.T) {
	t.Parallel()

	recorder := &directSSERecorder{}
	state := &requestState{
		stream:     true,
		modelName:  "claude-sonnet-4-6",
		blockCount: 8,
		directSSE:  recorder,
	}
	writer := NewSSEWriter(state, nil)
	if writer == nil {
		t.Fatal("expected direct SSE writer without onMessage callback")
	}

	writer.WriteError("bad_request", "boom")
	writer.WriteError("bad_request", "boom again")

	if len(recorder.writes) != 1 {
		t.Fatalf("len(writes)=%d want 1", len(recorder.writes))
	}
	if recorder.writes[0].event != "error" {
		t.Fatalf("writes[0].event=%q want error", recorder.writes[0].event)
	}
	if !recorder.writes[0].final {
		t.Fatal("error event should be final")
	}
	if len(recorder.finishCalls) != 1 {
		t.Fatalf("len(finishCalls)=%d want 1", len(recorder.finishCalls))
	}
	if recorder.finishCalls[0] != "error" {
		t.Fatalf("finishCalls[0]=%q want error", recorder.finishCalls[0])
	}
	if state.finishReason != "error" {
		t.Fatalf("state.finishReason=%q want error", state.finishReason)
	}
}
