package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
)

type diagnosticJournal struct{ events []audit.Event }

func (l *diagnosticJournal) Log(_ context.Context, e audit.Event) { l.events = append(l.events, e) }

func TestDiagnosticsUniqueIdentityAndUnifiedCompletion(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "identity:")
	journal := &diagnosticJournal{}
	previous, oldDetailed, oldSimple := requestJournal, detailedOutcomeRecorder, requestOutcomeRecorder
	defer func() {
		requestJournal = previous
		detailedOutcomeRecorder = oldDetailed
		requestOutcomeRecorder = oldSimple
	}()
	requestJournal = journal
	requestOutcomeRecorder = nil
	var durations []int64
	detailedOutcomeRecorder = func(_ context.Context, o opsagg.Outcome) {
		durations = append(durations, o.DurationMS)
		time.Sleep(40 * time.Millisecond)
	}
	observed := ObserveAuditLogger(journal)
	h := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "bad-json" {
			http.Error(w, "bad request", 400)
			return
		}
		// An existing handler's normal audit record must not create a duplicate row.
		observed.Log(r.Context(), audit.Event{Action: "grok_request", Kind: audit.KindRequest, Status: "success", Duration: 999, InputTokens: 2})
		w.Write(body)
	}))))
	ids := map[string]bool{}
	for i, body := range []string{"first", "second", "bad-json"} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		req.Header.Set(TraceIDHeader, "client-trace")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		id := rec.Header().Get(DiagnosticRequestIDHeader)
		if id == "" || id == "client-trace" || ids[id] {
			t.Fatalf("nonunique id %q", id)
		}
		ids[id] = true
		if rec.Header().Get(TraceIDHeader) != "client-trace" {
			t.Fatal("trace correlation changed")
		}
		b, err := store.Get(context.Background(), id)
		if err != nil || b == nil {
			t.Fatal(b, err)
		}
		if len(journal.events) != i+1 {
			t.Fatal("missing/duplicate journal rows", journal.events)
		}
		e := journal.events[i]
		if e.RequestID != id || b.DurationMS != durations[i] || e.Duration != b.DurationMS {
			t.Fatalf("duration or id mismatch: %+v %+v", b, e)
		}
		if i == 2 && (e.Status != "error" || e.Metadata["http_status"] != 400) {
			t.Fatal(e)
		}
		found := false
		for _, s := range b.Sections {
			if s.Name == "1_http_request.json" {
				found = s.Payload == body
			}
		}
		if !found {
			t.Fatal("wrong captured request")
		}
	}
	// Verify earlier bundles are still independently addressable after later saves.
	for id := range ids {
		if b, _ := store.Get(context.Background(), id); b == nil {
			t.Fatal("bundle overwritten")
		}
	}
}

func TestDiagnosticsStreamFailureSummaryMatchesJournal(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "failure:")
	journal := &diagnosticJournal{}
	previous := requestJournal
	requestJournal = journal
	defer func() { requestJournal = previous }()
	h := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: hello\n\n"))
		MarkStreamFailure(w)
	}))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", nil))
	b, _ := store.Get(context.Background(), rec.Header().Get(DiagnosticRequestIDHeader))
	if len(journal.events) != 1 || journal.events[0].Status != "stream_error" {
		t.Fatal(journal.events)
	}
	for _, s := range b.Sections {
		if s.Name == "6_http_summary.json" {
			var v map[string]interface{}
			json.Unmarshal([]byte(s.Payload), &v)
			if v["status"] != float64(200) || v["stream_failed"] != true {
				t.Fatal(v)
			}
		}
	}
}
