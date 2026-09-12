package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

// TestWarpAccountOutput_CarriesFingerprintNotSecret pins the contract behind the
// account table's Warp row: the browser session never leaves the server, but a
// short digest of it does, so two logins are distinguishable.
func TestWarpAccountOutput_CarriesFingerprintNotSecret(t *testing.T) {
	acc := &store.Account{
		ID:           7,
		AccountType:  "warp",
		RefreshToken: "warp-session-token-secret",
		Enabled:      true,
	}
	normalized := normalizeAccountOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeAccountOutput returned nil")
	}
	if normalized.SessionFingerprint == "" {
		t.Fatal("warp output has no session fingerprint")
	}
	if len(normalized.SessionFingerprint) != 12 {
		t.Fatalf("fingerprint = %q, want 12 hex characters", normalized.SessionFingerprint)
	}
	if strings.Contains(normalized.SessionFingerprint, "secret") {
		t.Fatalf("fingerprint leaked the token: %q", normalized.SessionFingerprint)
	}
	if normalized.RefreshToken != "" || normalized.Token != "" || normalized.ClientCookie != "" {
		t.Fatalf("warp credentials must stay redacted: %+v", normalized.Account)
	}

	// The same account type with a different session must fingerprint differently.
	other := normalizeAccountOutput(&store.Account{ID: 8, AccountType: "warp", RefreshToken: "another-session"})
	if other.SessionFingerprint == normalized.SessionFingerprint {
		t.Fatal("two different Warp sessions share a fingerprint")
	}

	// And the value must survive JSON encoding into the admin response.
	encoded, err := json.Marshal(normalized)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), "session_fingerprint") {
		t.Fatalf("fingerprint missing from the response: %s", encoded)
	}
	if strings.Contains(string(encoded), "warp-session-token-secret") {
		t.Fatalf("the response exposed the session token: %s", encoded)
	}
}

// TestWarpFingerprint_IsPresentInTheAccountList keeps the wiring honest: the
// endpoint the table calls is the one that must carry it.
func TestWarpFingerprint_IsPresentInTheAccountList(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{ID: 0, Name: "warp-login", AccountType: "warp", RefreshToken: "session-abc", Enabled: true, Weight: 1}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, row := range rows {
		if row["account_type"] != "warp" {
			continue
		}
		found = true
		fingerprint, _ := row["session_fingerprint"].(string)
		if fingerprint == "" {
			t.Fatalf("warp row has no session_fingerprint: %v", row)
		}
		if _, leaked := row["refresh_token"].(string); leaked {
			if row["refresh_token"] != "" {
				t.Fatalf("warp row exposed the session token: %v", row["refresh_token"])
			}
		}
	}
	if !found {
		t.Fatal("no warp row in the list response")
	}
}
