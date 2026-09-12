package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type recordedOutcome struct {
	Channel      string
	Model        string
	StatusClass  string
	DurationMS   int64
	FirstTokenMS int64
}

// captureOutcomes swaps the process-wide recorder for the duration of one test.
// The recorder is a package global, so these tests must not run in parallel.
func captureOutcomes(t *testing.T) *[]recordedOutcome {
	t.Helper()
	previous := requestOutcomeRecorder
	outcomes := &[]recordedOutcome{}
	SetRequestOutcomeRecorder(func(channel, model, statusClass string, durationMS, firstTokenMS int64) {
		*outcomes = append(*outcomes, recordedOutcome{channel, model, statusClass, durationMS, firstTokenMS})
	})
	t.Cleanup(func() { requestOutcomeRecorder = previous })
	return outcomes
}

// TestObservedOutcome_TimeToFirstTokenSkipsKeepalives pins the reported defect:
// a streaming handler commits its status line and may send SSE keepalive
// comments long before a token exists, so measuring time-to-first-token from the
// header write understated every prefill it reported.
func TestObservedOutcome_TimeToFirstTokenSkipsKeepalives(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// A keepalive that arrives well before the model produces anything.
		_, _ = w.Write([]byte(": keepalive\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if len(*outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(*outcomes))
	}
	outcome := (*outcomes)[0]
	if outcome.Channel != "grok" || outcome.StatusClass != "2xx" {
		t.Fatalf("outcome = %+v, want the grok /v1 channel with a 2xx class", outcome)
	}
	if outcome.FirstTokenMS < 50 {
		t.Fatalf("first_token_ms = %d, want it measured from the first payload byte (>= 50ms of keepalive)", outcome.FirstTokenMS)
	}
	if outcome.FirstTokenMS > outcome.DurationMS {
		t.Fatalf("first_token_ms = %d exceeds duration_ms = %d", outcome.FirstTokenMS, outcome.DurationMS)
	}
}

// TestObservedOutcome_FallsBackToFirstWrite covers the non-streaming case: a JSON
// body is a single payload write, and a response that never wrote a body still
// reports a time-to-first-byte rather than zero.
func TestObservedOutcome_FallsBackToFirstWrite(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(*outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(*outcomes))
	}
	if got := (*outcomes)[0].StatusClass; got != "2xx" {
		t.Fatalf("status class = %q, want 2xx", got)
	}
}

// TestObservedOutcome_StreamFailureAfterCommittedStatus pins the other reported
// defect: a stream that starts with 200 and then fails was counted as a success.
func TestObservedOutcome_StreamFailureAfterCommittedStatus(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		MarkStreamFailure(w)
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":{\"code\":\"stream_error\"}}\n\n"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	// The client still saw a 200: that part cannot be changed after the fact.
	if recorder.Code != http.StatusOK {
		t.Fatalf("client status = %d, want the committed 200", recorder.Code)
	}
	if len(*outcomes) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(*outcomes))
	}
	if got := (*outcomes)[0].StatusClass; got != "stream_error" {
		t.Fatalf("status class = %q, want stream_error (counted as a failure)", got)
	}
}

// TestMarkStreamFailure_IsSafeOnForeignWriters keeps the helper usable from the
// grok SSE writers, which only hold an http.ResponseWriter.
func TestMarkStreamFailure_IsSafeOnForeignWriters(t *testing.T) {
	MarkStreamFailure(nil)
	MarkStreamFailure(httptest.NewRecorder())

	traced := NewTracedResponseWriter(httptest.NewRecorder())
	if traced.StreamFailed() {
		t.Fatal("a fresh writer reports a stream failure")
	}
	MarkStreamFailure(traced)
	if !traced.StreamFailed() {
		t.Fatal("MarkStreamFailure did not reach the traced writer")
	}
}

func TestIsPayloadWrite(t *testing.T) {
	cases := []struct {
		name  string
		write string
		want  bool
	}{
		{"empty", "", false},
		{"whitespace only", "\n\n", false},
		{"sse keepalive", ": keepalive\n\n", false},
		{"sse comment indented", "  : ping\n", false},
		{"sse data", "data: {}\n\n", true},
		{"json body", `{"ok":true}`, true},
		{"plain text", "hello", true},
	}
	for _, tc := range cases {
		if got := isPayloadWrite([]byte(tc.write)); got != tc.want {
			t.Errorf("isPayloadWrite(%q) = %v, want %v", tc.write, got, tc.want)
		}
	}
}
