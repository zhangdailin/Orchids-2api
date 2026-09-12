package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/audit"
)

type recordingAuditLogger struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *recordingAuditLogger) Log(_ context.Context, event audit.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingAuditLogger) snapshot() []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]audit.Event(nil), l.events...)
}

// TestAdminSessionAudit_RecordsMutationsOnly keeps the journal useful: a read
// would bury the changes an operator needs to trace.
func TestAdminSessionAudit_RecordsMutationsOnly(t *testing.T) {
	logger := &recordingAuditLogger{}
	SetOperationAuditLogger(logger)
	t.Cleanup(func() { SetOperationAuditLogger(nil) })

	handler := adminSessionAudit(func(w http.ResponseWriter, r *http.Request) {
		// The handler must still receive the body after it was captured.
		if r.Body != nil {
			buf := make([]byte, 64)
			n, _ := r.Body.Read(buf)
			if n == 0 && r.Method == http.MethodPost {
				t.Error("handler received an empty body")
			}
		}
		w.WriteHeader(http.StatusCreated)
	})

	readReq := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	handler(httptest.NewRecorder(), readReq)

	writeReq := httptest.NewRequest(http.MethodPut, "/api/accounts/42", strings.NewReader(`{"weight":2,"client_cookie":"sso=secret"}`))
	writeReq.Header.Set("Content-Type", "application/json")
	writeReq.AddCookie(&http.Cookie{Name: "session_token", Value: "x"})
	handler(httptest.NewRecorder(), writeReq)

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want only the mutating call: %+v", len(events), events)
	}
	event := events[0]
	if event.Kind != audit.KindOperation {
		t.Fatalf("kind = %q, want operation", event.Kind)
	}
	if event.Action != "accounts.42.update" && !strings.HasSuffix(event.Action, ".update") {
		t.Fatalf("action = %q", event.Action)
	}
	if event.Status != "success" {
		t.Fatalf("status = %q", event.Status)
	}
	if event.Actor != "admin-session" {
		t.Fatalf("actor = %q, want the browser session", event.Actor)
	}
	if event.Target == "" {
		t.Fatal("target should name the changed object")
	}
	if strings.Contains(event.Details, "sso=secret") {
		t.Fatalf("details leaked a credential: %s", event.Details)
	}
	if len(event.Redacted) == 0 {
		t.Fatal("the masked field should be reported")
	}
}

// TestAdminSessionAudit_LoginBodyIsNeverRecorded: the login payload is the admin
// password, so it must not be summarised at all.
func TestAdminSessionAudit_LoginBodyIsNeverRecorded(t *testing.T) {
	logger := &recordingAuditLogger{}
	SetOperationAuditLogger(logger)
	t.Cleanup(func() { SetOperationAuditLogger(nil) })

	handler := adminSessionAudit(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"hunter2"}`))
	req.Header.Set("Content-Type", "application/json")
	handler(httptest.NewRecorder(), req)

	events := logger.snapshot()
	if len(events) != 1 {
		t.Fatalf("events = %d, want the login attempt recorded", len(events))
	}
	if strings.Contains(events[0].Details, "hunter2") || events[0].Details != "" {
		t.Fatalf("login body leaked into the journal: %q", events[0].Details)
	}
}

// TestAdminSessionAudit_ErrorResult pinpoints a failed change attempt.
func TestAdminSessionAudit_ErrorResult(t *testing.T) {
	logger := &recordingAuditLogger{}
	SetOperationAuditLogger(logger)
	t.Cleanup(func() { SetOperationAuditLogger(nil) })

	handler := adminSessionAudit(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing sso token", http.StatusBadRequest)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"account_type":"grok"}`))
	handler(httptest.NewRecorder(), req)

	events := logger.snapshot()
	if len(events) != 1 || events[0].Status != "error" {
		t.Fatalf("events = %+v", events)
	}
	if code, _ := events[0].Metadata["code"].(int); code != http.StatusBadRequest {
		t.Fatalf("metadata code = %v, want 400", events[0].Metadata["code"])
	}
}

// TestOperationAction_Naming is the mechanical naming contract.
func TestOperationAction_Naming(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{http.MethodPost, "/api/accounts", "accounts.create"},
		{http.MethodPut, "/api/accounts/7", "accounts.7.update"},
		{http.MethodDelete, "/api/keys/3", "keys.3.delete"},
		{http.MethodPost, "/api/login", "session.create"},
		{http.MethodPost, "/api/config", "config.create"},
		{http.MethodGet, "/api/audit", "audit.read"},
	}
	for _, tc := range cases {
		if got := operationAction(tc.method, tc.path); got != tc.want {
			t.Fatalf("operationAction(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestOperationTarget prefers the addressed object over a query parameter.
func TestOperationTarget(t *testing.T) {
	if got := operationTarget("/api/accounts/42", nil); got != "accounts:42" {
		t.Fatalf("target = %q", got)
	}
	if got := operationTarget("/api/accounts", map[string][]string{"account_id": {"9"}}); got != "account_id:9" {
		t.Fatalf("target = %q", got)
	}
	if got := operationTarget("/api/config", nil); got != "" {
		t.Fatalf("target = %q, want empty for a singleton resource", got)
	}
}
