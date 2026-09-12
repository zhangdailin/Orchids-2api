package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

// TestGrokSSOCredentialUpdateClearsStaleUnauthorizedStatus covers the reported
// recovery path: an account whose SSO cookie was rejected shows 未授权, the
// operator pastes a working cookie, and the badge must stop claiming the NEW
// credential is unauthorized. The old status reason used to be pinned by the
// edit handler, so the account kept displaying 未授权 until a manual Sync.
func TestGrokSSOCredentialUpdateClearsStaleUnauthorizedStatus(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		Name:           "grok-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   grok.ProviderWeb,
		ClientCookie:   "sso=token-stale",
		Enabled:        true,
		Weight:         1,
		StatusCode:     "401",
		StatusMessage:  "failed to verify grok account: 401: grok session unauthenticated",
		VerifiedAt:     time.Now().Add(-time.Minute),
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"account_type":  "grok",
		"client_cookie": "sso=token-fixed",
		"enabled":       true,
		"weight":        1,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d body=%s", rec.Code, rec.Body.String())
	}

	after, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.ClientCookie != "sso=token-fixed" {
		t.Fatalf("cookie = %q, want the replacement", after.ClientCookie)
	}
	if after.StatusCode != "" || after.StatusMessage != "" {
		t.Fatalf("replacement credential inherited stale status: %q / %q", after.StatusCode, after.StatusMessage)
	}
	if !after.LastAttempt.IsZero() {
		t.Fatalf("last_attempt = %v, want reset so the new credential is re-verified", after.LastAttempt)
	}
	if !after.VerifiedAt.IsZero() {
		t.Fatalf("verified_at = %v, want cleared so the new credential is verified instead of trusted", after.VerifiedAt)
	}
}

// TestGrokSSOUnchangedCredentialEditKeepsObservedStatus is the counterweight: a
// settings-only save must not silently erase a real observation.
func TestGrokSSOUnchangedCredentialEditKeepsObservedStatus(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		Name:           "grok-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   grok.ProviderWeb,
		ClientCookie:   "sso=token-kept",
		Enabled:        true,
		Weight:         1,
		StatusCode:     "429",
		StatusMessage:  "quota exceeded",
		VerifiedAt:     time.Now().Add(-2 * time.Minute),
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"account_type":  "grok",
		"client_cookie": "sso=token-kept",
		"enabled":       true,
		"weight":        3,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d body=%s", rec.Code, rec.Body.String())
	}

	after, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.Weight != 3 {
		t.Fatalf("weight = %d, want the edit applied", after.Weight)
	}
	if after.StatusCode != "429" || after.StatusMessage != "quota exceeded" {
		t.Fatalf("settings-only edit erased the observed status: %q / %q", after.StatusCode, after.StatusMessage)
	}
	if after.VerifiedAt.IsZero() {
		t.Fatal("settings-only edit erased the verdict timestamp")
	}
}
