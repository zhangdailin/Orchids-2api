package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/audit"

	"github.com/redis/go-redis/v9"
)

// seedJournal writes raw journal entries into the audit stream in the order the
// real writer uses: an upstream attempt is written BEFORE the request it belongs
// to, and the stream is read newest-first.
func seedJournal(t *testing.T, a *API, events []audit.Event) {
	t.Helper()
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"
	for _, event := range events {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now()
		}
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := client.XAdd(context.Background(), &redis.XAddArgs{
			Stream: key,
			Values: map[string]interface{}{"data": string(raw), "action": event.Action, "status": event.Status, "kind": string(event.Kind)},
		}).Result(); err != nil {
			t.Fatalf("XAdd: %v", err)
		}
	}
}

func journalRequest(t *testing.T, a *API, query string) map[string]interface{} {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/journal/records"+query, nil)
	recorder := httptest.NewRecorder()
	a.HandleJournalRecords(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("journal status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("journal body is not JSON: %v (%s)", err, recorder.Body.String())
	}
	return payload
}

// TestJournalRecords_AttachAttemptsToTheirRequest pins the reported defect that
// every detail panel came back empty. Attempts are written before their request,
// so a reverse-order scan sees them AFTER it; attaching them in a single pass
// could never find them.
func TestJournalRecords_AttachAttemptsToTheirRequest(t *testing.T) {
	s, _ := newTestStore(t, "journal-attempts:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	seedJournal(t, a, []audit.Event{
		{Kind: audit.KindRequest, Action: "grok_upstream_attempt", RequestID: "req-1", Status: "error", Channel: "grok"},
		{Kind: audit.KindRequest, Action: "grok_upstream_attempt", RequestID: "req-1", Status: "success", Channel: "grok"},
		{Kind: audit.KindRequest, Action: "grok_request", RequestID: "req-1", Status: "ok", Channel: "grok", Model: "grok-4.6"},
	})

	payload := journalRequest(t, a, "?kind=request")
	rows, ok := payload["data"].([]interface{})
	if !ok {
		t.Fatalf("data = %T, want a list", payload["data"])
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (attempts are shown through their request)", len(rows))
	}
	row, _ := rows[0].(map[string]interface{})
	attempts, _ := row["attempts"].([]interface{})
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want the 2 upstream tries of req-1", len(attempts))
	}
	event, _ := row["event"].(map[string]interface{})
	if event["request_id"] != "req-1" {
		t.Fatalf("row event request_id = %v, want req-1", event["request_id"])
	}
}

// TestJournalRecords_CursorAdvancesPastScannedEntries pins the second half of the
// defect: returning the last MATCHING record as the cursor made older pages
// unreachable whenever the window held more non-matching entries than one page —
// with a full page of non-matching entries the cursor came back empty and the
// page holding the record could not be requested at all.
func TestJournalRecords_CursorAdvancesPastScannedEntries(t *testing.T) {
	s, _ := newTestStore(t, "journal-cursor:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	// The one request this test looks for is the OLDEST entry, buried under 40
	// newer operations: exactly the "earlier operation logs cannot be found" case.
	events := []audit.Event{{Kind: audit.KindRequest, Action: "grok_request", RequestID: "req-old", Status: "ok", Channel: "grok"}}
	for i := 0; i < 40; i++ {
		events = append(events, audit.Event{Kind: audit.KindOperation, Action: "config_update", Status: "ok", Details: "tweak"})
	}
	seedJournal(t, a, events)

	// limit=1 makes the scan window (limit*6) smaller than the 41 stored entries.
	first := journalRequest(t, a, "?kind=request&limit=1")
	if rows, _ := first["data"].([]interface{}); len(rows) != 0 {
		t.Fatalf("first page rows = %d, want 0 (newest 6 entries are operations)", len(rows))
	}
	// The cursor must point past everything that was scanned, so following it
	// eventually reaches the request the first page could not fit.
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor is empty although the scan window was exhausted: older pages become unreachable")
	}
	if scanned, _ := first["scanned"].(float64); scanned <= 0 {
		t.Fatalf("scanned = %v, want the number of entries the page examined", first["scanned"])
	}
	if matched, _ := first["matched"].(float64); matched != 0 {
		t.Fatalf("matched = %v, want 0 on a page of operations", first["matched"])
	}

	found := false
	seen := map[string]bool{}
	for page := 0; page < 12 && cursor != "" && !found; page++ {
		if seen[cursor] {
			t.Fatalf("page %d reused cursor %s: paging is stuck", page, cursor)
		}
		seen[cursor] = true
		payload := journalRequest(t, a, "?kind=request&limit=1&before="+cursor)
		rows, _ := payload["data"].([]interface{})
		for _, raw := range rows {
			row, _ := raw.(map[string]interface{})
			event, _ := row["event"].(map[string]interface{})
			if event["request_id"] == "req-old" {
				found = true
			}
		}
		cursor, _ = payload["next_cursor"].(string)
	}
	if !found {
		t.Fatal("following next_cursor never reached the buried request: older journal entries are unreachable")
	}
}
