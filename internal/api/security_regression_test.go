package api

import (
	"encoding/json"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"strings"
	"testing"
)

func TestAccountCredentialsAreWriteOnly(t *testing.T) {
	for _, provider := range []string{"qoder", "grok", "cline", "workbuddy"} {
		acc := &store.Account{ID: 9, AccountType: provider, Token: "private-token", ClientCookie: "private-cookie", RefreshToken: "private-refresh", SessionCookie: "private-session", SessionID: "private-session-id", ClientUat: "private-uat", OAuthAccessToken: "private-oauth", OAuthRefreshToken: "private-oauth-refresh", WorkBuddyAccessToken: "private-wb", WorkBuddyRefreshToken: "private-wb-refresh", QoderAccessToken: "private-qoder", QoderRefreshToken: "private-qoder-refresh", ClineAccessToken: "private-cline", ClineRefreshToken: "private-cline-refresh", StatusMessage: "upstream rejected private-refresh"}
		raw, err := json.Marshal(normalizeAccountOutput(acc))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "private-") {
			t.Fatalf("credential leaked for %s", provider)
		}
		var row map[string]interface{}
		_ = json.Unmarshal(raw, &row)
		if row["has_credential"] != true {
			t.Fatalf("missing credential presence for %s", provider)
		}
		for _, key := range []string{"token", "client_cookie", "refresh_token", "session_cookie", "session_id", "client_uat", "oauth_access_token", "oauth_refresh_token", "workbuddy_access_token", "workbuddy_refresh_token", "qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key", "cline_access_token", "cline_refresh_token", "session_fingerprint"} {
			if _, exists := row[key]; exists {
				t.Fatalf("credential field %s was returned", key)
			}
		}
		if acc.RefreshToken != "private-refresh" {
			t.Fatal("redaction changed stored account")
		}
	}
}

func TestDiagnosticTogglePersistsAndFailsSafely(t *testing.T) {
	s, mini := newTestStore(t, "diagnostic-toggle:")
	defer s.Close()
	cfg := &config.Config{AdminPass: "test-admin-secret"}
	a := New(s, "admin", cfg.AdminPass, cfg)
	for _, enabled := range []string{"true", "false"} {
		rec := httptest.NewRecorder()
		a.HandleDiagnosticSettings(rec, httptest.NewRequest("PUT", "/api/journal/diagnostics/settings", strings.NewReader(`{"enabled":`+enabled+`}`)))
		if rec.Code != 200 || a.DiagnosticsEnabled() != (enabled == "true") {
			t.Fatalf("toggle failed: %d", rec.Code)
		}
		stored, err := s.GetSetting(t.Context(), "config")
		if err != nil || !strings.Contains(stored, `"debug_enabled":`+enabled) {
			t.Fatal("toggle was not persisted")
		}
	}
	mini.SetError("storage unavailable")
	rec := httptest.NewRecorder()
	a.HandleDiagnosticSettings(rec, httptest.NewRequest("PUT", "/api/journal/diagnostics/settings", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != 503 || a.DiagnosticsEnabled() {
		t.Fatal("failed save changed runtime setting")
	}
	rec = httptest.NewRecorder()
	a.HandleDiagnosticSettings(rec, httptest.NewRequest("GET", "/api/journal/diagnostics/settings", nil))
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "admin") {
		t.Fatal("settings exposed unrelated secrets")
	}
}
