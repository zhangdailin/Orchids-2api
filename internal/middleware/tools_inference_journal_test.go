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
)

// The Grok tools page runs inference on the session-authenticated admin
// namespace /api/grok/tools/v1/*. Before the channel mapping knew that prefix,
// each call there was classified as plain HTTP traffic: no request row reached
// the log centre and no diagnostic bundle was ever captured for it.
func TestRequestChannelAttributesAdminToolsInference(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/api/grok/tools/v1/responses", "grok"},
		{"/api/grok/tools/v1/chat/completions", "grok"},
		{"/api/grok/tools/v1/images/generations", "grok"},
		{"/api/grok/tools/v1/videos/generations", "grok"},
		{"/api/grok/tools/v1/audio/speech", "grok"},
		{"/api/grok/tools/v1/tts", "grok"},
		// Model discovery and the management endpoints stay HTTP traffic.
		{"/api/grok/tools/v1/models", HTTPChannel},
		{"/api/grok/models", HTTPChannel},
		{"/api/accounts", HTTPChannel},
	}
	for _, tc := range cases {
		if got := requestChannel(tc.path); got != tc.want {
			t.Errorf("requestChannel(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestAdminToolsInferenceIsCapturedInTheLogCentre(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "test:")
	handler := TraceMiddleware(Diagnostics(store, func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The tee captures the body the handler actually reads, exactly as the
		// native Grok handler does before it parses the request.
		if body, err := io.ReadAll(r.Body); err != nil || len(body) == 0 {
			t.Errorf("handler did not see the request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1"}`))
	})))
	body := `{"model":"grok-4.20-0309","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/grok/tools/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	requestID := recorder.Header().Get(TraceIDHeader)
	if requestID == "" {
		t.Fatal("no request id was published")
	}
	bundle, err := store.Get(context.Background(), requestID)
	if err != nil || bundle == nil {
		t.Fatalf("no diagnostics for an admin tools request: bundle=%v err=%v", bundle, err)
	}
	found := false
	for _, section := range bundle.Sections {
		if section.Name == "1_http_request.json" && strings.Contains(section.Payload, `"model":"grok-4.20-0309"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostic bundle does not carry the request body: %#v", bundle.Sections)
	}
}

// The log centre lists one request row per inference call, and the diagnostics
// hang off that row. The admin tools namespace produced no row at all, so an
// operator could not open the diagnostics of the request the tools page had
// just made.
func TestAdminToolsInferenceWritesARequestJournalRow(t *testing.T) {
	journal := &diagnosticJournal{}
	previous := requestJournal
	defer func() { requestJournal = previous }()
	requestJournal = journal
	// The request row is written by LoggingMiddleware, which is the innermost
	// layer of the production chain (TraceMiddleware → Diagnostics → Logging).
	handler := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	req := httptest.NewRequest(http.MethodPost, "/api/grok/tools/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(journal.events) != 1 {
		t.Fatalf("request journal rows = %d, want 1: %#v", len(journal.events), journal.events)
	}
	event := journal.events[0]
	if event.Kind != audit.KindRequest || event.Channel != "grok" || event.RequestID == "" {
		t.Fatalf("journal row = %+v", event)
	}
}
