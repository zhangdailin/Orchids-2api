package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

// TestHandleAccountsListIsTheSingleSidebarTruth pins the contract the sidebar
// counter now depends on. The server used to render its own 总账号/正常/异常 in
// index.html while accounts.js and common.js each computed a different verdict,
// so the same sidebar read 5 on 账号管理 and 11 on 运维总览. The renderer no longer
// publishes a number; GET /api/accounts is the only input, which means it must
// keep excluding the internal Console companion that backs a Web SSO login.
func TestHandleAccountsListIsTheSingleSidebarTruth(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()

	source := &store.Account{
		AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
		ClientCookie: "sso=visible-source", Enabled: true,
	}
	if err := s.CreateAccount(ctx, source); err != nil {
		t.Fatalf("CreateAccount(source) error = %v", err)
	}
	companion := &store.Account{
		AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole,
		GrokSSOParentID: source.ID, ClientCookie: "sso=visible-source", Enabled: true,
	}
	if err := s.CreateAccount(ctx, companion); err != nil {
		t.Fatalf("CreateAccount(companion) error = %v", err)
	}

	recorder := httptest.NewRecorder()
	a.HandleAccounts(recorder, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("account rows = %d, want 1: the linked Console companion must not be countable", len(rows))
	}
	if id, _ := rows[0]["id"].(float64); int64(id) != source.ID {
		t.Fatalf("surviving row id = %v, want the visible Web source %d", rows[0]["id"], source.ID)
	}
	if _, ok := rows[0]["has_credential"].(bool); !ok {
		t.Fatalf("row is missing the has_credential flag the sidebar predicate reads: %v", rows[0])
	}
}
