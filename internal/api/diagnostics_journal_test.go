package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/middleware"
)

func TestInvalidJSONDiagnosticIsReachableFromJournal(t *testing.T) {
	s, _ := newTestStore(t, "invalid-json-diagnostic:")
	defer s.Close()
	d := debug.NewDiagnosticStore(s.RedisClient(), s.RedisPrefix())
	a := &API{store: s}
	a.SetDiagnosticStore(d)
	journal := audit.NewRedisLogger(s.RedisClient(), s.RedisPrefix(), 100)
	middleware.SetRequestAuditLogger(journal)
	defer middleware.SetRequestAuditLogger(nil)
	h := new(handler.Handler)
	h.SetAuditLogger(middleware.ObserveAuditLogger(journal))
	wrapped := middleware.TraceMiddleware(middleware.Diagnostics(d, func() bool { return true })(middleware.LoggingMiddleware(http.HandlerFunc(h.HandleMessages))))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("POST", "/workbuddy/v1/messages", strings.NewReader("bad-json")))
	journal.Close() // Flush the asynchronous audit writer before querying the API.
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	list := journalRequest(t, a, "?kind=request&channel=workbuddy")
	rows := list["data"].([]interface{})
	if len(rows) != 1 {
		t.Fatal(list)
	}
	row := rows[0].(map[string]interface{})
	event := row["event"].(map[string]interface{})
	id := rec.Header().Get(middleware.DiagnosticRequestIDHeader)
	if event["request_id"] != id || row["diagnostics"] == nil {
		t.Fatal(row)
	}
	detail := httptest.NewRecorder()
	a.HandleJournalDiagnostics(detail, httptest.NewRequest("GET", "/api/journal/diagnostics?request_id="+id, nil))
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "bad-json") {
		t.Fatal(detail.Code, detail.Body.String())
	}
}
