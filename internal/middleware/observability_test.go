package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
)

func TestDiagnosticWriterBoundsBufferedResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &diagnosticWriter{TracedResponseWriter: NewTracedResponseWriter(recorder)}
	payload := strings.Repeat("x", maxDiagnosticResponseBytes*3)
	if n, err := writer.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(payload))
	}
	if recorder.Body.Len() != len(payload) {
		t.Fatalf("client response bytes = %d, want %d", recorder.Body.Len(), len(payload))
	}
	if writer.body.Len() != maxDiagnosticResponseBytes {
		t.Fatalf("diagnostic buffer bytes = %d, want cap %d", writer.body.Len(), maxDiagnosticResponseBytes)
	}
}

func TestDiagnosticsStreamingAndExactlyOneOutcome(t *testing.T) {
	old := detailedOutcomeRecorder
	defer func() { detailedOutcomeRecorder = old }()
	var outcomes []opsagg.Outcome
	SetDetailedOutcomeRecorder(func(_ context.Context, o opsagg.Outcome) { outcomes = append(outcomes, o) })
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "test:")
	log := ObserveAuditLogger(audit.NewNopLogger())
	original := `{"model":"test","messages":[{"content":"hello"}]}`
	handler := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != original {
			t.Error("request changed")
		}
		r = r.WithContext(WithRequestModel(r.Context(), "test"))
		log.Log(r.Context(), audit.Event{Action: "grok_upstream_attempt", AccountID: 1, Status: "error"})
		log.Log(r.Context(), audit.Event{Action: "grok_upstream_attempt", AccountID: 2, Status: "success"})
		log.Log(r.Context(), audit.Event{Action: "grok_request", Status: "ok", InputTokens: 11, OutputTokens: 22})
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		MarkStreamFailure(w)
	}))))
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(original))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Body.String() != "data: hello\n\n" || !recorder.Flushed {
		t.Fatal("streaming changed")
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes=%d", len(outcomes))
	}
	o := outcomes[0]
	if o.OK || o.Status != "stream_error" || o.InputTokens != 11 || o.OutputTokens != 22 || o.AttemptFailures != 1 || o.AccountSwitches != 1 || o.Model != "test" {
		t.Fatalf("outcome=%+v", o)
	}
	b, err := store.Get(context.Background(), recorder.Header().Get(TraceIDHeader))
	if err != nil || b == nil {
		t.Fatalf("diagnostics=%v err=%v", b, err)
	}
	found, summaryFound := false, false
	for _, s := range b.Sections {
		if s.Name == "6_http_summary.json" && strings.Contains(s.Payload, `"stream_failed":true`) {
			summaryFound = true
		}
		if s.Name == "5_http_response.txt" && s.Payload == recorder.Body.String() {
			found = true
		}
	}
	if !found || !summaryFound {
		t.Fatal("response diagnostic missing")
	}
}

func TestDiagnosticsOmitsSuccessfulStructuredRawResponse(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "test:")
	handler := TraceMiddleware(Diagnostics(store, func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\\ndata: {}\\n\\n")
	})))
	req := httptest.NewRequest("POST", "/cline/v1/messages", strings.NewReader(`{"model":"test","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	b, err := store.Get(context.Background(), recorder.Header().Get(TraceIDHeader))
	if err != nil || b == nil {
		t.Fatalf("diagnostics=%v err=%v", b, err)
	}
	for _, section := range b.Sections {
		if section.Name == "5_http_response.txt" {
			t.Fatalf("successful structured response should not be duplicated: %q", section.Payload)
		}
	}
}
