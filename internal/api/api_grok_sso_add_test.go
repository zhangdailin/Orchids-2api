package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// grokSSOUpstream stubs the Web SSO endpoints an account sync touches. reject
// lists the cookies the session endpoint refuses (by substring).
func grokSSOUpstream(t *testing.T, reject ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		cookie := r.Header.Get("Cookie")
		for _, needle := range reject {
			if needle != "" && strings.Contains(cookie, needle) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
				return
			}
		}
		switch r.URL.Path {
		case "/api/auth/session":
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-ok","email":"ok@example.com","organizationId":"team-ok"}}`))
		case "/rest/rate-limits":
			_, _ = w.Write([]byte(`{"remainingQueries":100,"totalQueries":100,"remainingTokens":1000,"totalTokens":1000,"windowSizeSeconds":3600}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}

// TestHandleAccounts_PostSSOAccountDoesNotDisturbExistingSSOAccount reproduces the
// reported sequence: an existing Grok SSO account must keep its credential and
// health when a SECOND SSO account is added, even when the new cookie is rejected
// by the upstream.
func TestHandleAccounts_PostSSOAccountDoesNotDisturbExistingSSOAccount(t *testing.T) {
	srv := grokSSOUpstream(t, "sso=token-bad")
	defer srv.Close()

	s, _ := newTestStore(t, "grok-sso-add:")
	ctx := context.Background()
	a := New(s, "", "", &config.Config{GrokAPIBaseURL: srv.URL})

	first := &store.Account{
		Name:           "first-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=token-good",
		Enabled:        true,
		Weight:         1,
	}
	if err := s.CreateAccount(ctx, first); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	// Bring the first account to a healthy, synced state.
	if _, _, err := a.refreshAccountState(ctx, first); err != nil {
		t.Fatalf("initial refreshAccountState() error = %v", err)
	}
	if err := s.UpdateAccount(ctx, first); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}
	before, _ := s.GetAccount(ctx, first.ID)
	if before.StatusCode != "" {
		t.Fatalf("precondition: first account status = %q, want clean", before.StatusCode)
	}
	if before.UserID != "user-ok" || before.TeamID != "team-ok" {
		t.Fatalf("precondition: first account identity not synced: user=%q team=%q", before.UserID, before.TeamID)
	}

	// Add the second SSO account through the real admin endpoint.
	body, _ := json.Marshal(map[string]interface{}{
		"account_type":  "grok",
		"client_cookie": "sso=token-bad",
		"enabled":       true,
		"weight":        1,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	t.Logf("create second account: status=%d body=%s", rec.Code, rec.Body.String())

	// A credential the upstream refuses must be rejected outright rather than
	// persisted as a healthy account that fails every routed request.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("create second account status = %d, want 401 (rejected credential)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("create second account body = %s, want authentication_error", rec.Body.String())
	}

	// The first account must be untouched, in every observable dimension.
	after, err := s.GetAccount(ctx, first.ID)
	if err != nil {
		t.Fatalf("the existing SSO account disappeared: %v", err)
	}
	if after.ClientCookie != before.ClientCookie {
		t.Fatalf("existing SSO cookie changed: %q -> %q", before.ClientCookie, after.ClientCookie)
	}
	if after.StatusCode != before.StatusCode || after.StatusMessage != before.StatusMessage {
		t.Fatalf("existing SSO status changed: %q/%q -> %q/%q",
			before.StatusCode, before.StatusMessage, after.StatusCode, after.StatusMessage)
	}
	if after.Enabled != before.Enabled || after.UserID != before.UserID || after.TeamID != before.TeamID {
		t.Fatalf("existing SSO identity changed: enabled=%v user=%q team=%q", after.Enabled, after.UserID, after.TeamID)
	}
	if after.GrokSSOParentID != 0 || after.GrokProvider != "web" || after.CredentialType != "sso" {
		t.Fatalf("existing SSO account was reclassified: %+v", after)
	}

	// The rejected account must have been rolled back; only the first remains.
	listRec := httptest.NewRecorder()
	a.HandleAccounts(listRec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	var rows []map[string]interface{}
	if err := json.Unmarshal(listRec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("account rows = %d, want 1 (rejected SSO account rolled back, first account kept)", len(rows))
	}
	for _, row := range rows {
		if id, _ := row["id"].(float64); int64(id) == first.ID {
			if row["status_code"] != "" {
				t.Fatalf("existing account row status = %v, want clean", row["status_code"])
			}
			if token, _ := row["client_cookie"].(string); token != "sso=token-good" {
				t.Fatalf("existing account row lost its cookie: %q", token)
			}
		}
	}
}
