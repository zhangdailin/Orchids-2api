package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// The account export is a downloadable, shareable artifact, so it is a different
// surface from the account API: it marshals the stored record directly to stay
// re-importable, which means it does not pass through accountOutput.MarshalJSON
// and its key-stripping. These tests pin the guarantees that gap would otherwise
// leave unstated.

// exportedCredentialKeys exports one account of the given channel and returns the
// JSON keys whose value is one of the planted secrets.
func exportedCredentialKeys(t *testing.T, acc *store.Account) []string {
	t.Helper()
	s, _ := newTestStore(t, "export-guard-"+acc.AccountType+":")
	defer s.Close()
	cfg := &config.Config{AdminPass: "x"}
	a := New(s, "admin", cfg.AdminPass, cfg)
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatalf("create account: %v", err)
	}
	rec := httptest.NewRecorder()
	a.HandleExport(rec, httptest.NewRequest(http.MethodGet, "/api/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	keys := []string{}
	for _, row := range payload.Accounts {
		for key, value := range row {
			if text, ok := value.(string); ok && strings.HasPrefix(text, marker) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// TestExportNeverCarriesAnotherChannelsCredential is the guard for the gap.
//
// A legacy row can hold a value in a slot its own channel never writes. The
// account API hides that by stripping credential keys from the response; the
// export has to hide it in data instead, because it marshals the record itself.
// The planted account deliberately carries a credential in every channel's slots,
// which is exactly the shape a row migrated from an older release can have.
func TestExportNeverCarriesAnotherChannelsCredential(t *testing.T) {
	// Which planted slots each channel may legitimately export. The generic slots
	// are included because every channel's resolver may fall back to them for a
	// credential document written before the channel had fields of its own.
	allowed := map[string]map[string]bool{
		"grok": setOf("client_cookie", "refresh_token", "token", "session_cookie",
			"session_id", "client_uat", "oauth_access_token", "oauth_refresh_token"),
		"puter": setOf("client_cookie", "refresh_token", "token", "session_cookie",
			"session_id", "client_uat"),
		"workbuddy": setOf("client_cookie", "refresh_token", "token", "session_cookie",
			"session_id", "client_uat", "workbuddy_access_token", "workbuddy_refresh_token"),
		"qoder": setOf("client_cookie", "refresh_token", "token", "session_cookie",
			"session_id", "client_uat", "qoder_access_token", "qoder_refresh_token",
			"qoder_runtime_info", "qoder_runtime_key"),
	}
	for _, channel := range []string{"grok", "puter", "workbuddy", "qoder"} {
		t.Run(channel, func(t *testing.T) {
			acc := &store.Account{
				ID: 1, Name: "guard", AccountType: channel, Enabled: true, Weight: 1,
				Token: marker + "token", ClientCookie: marker + "client_cookie",
				RefreshToken: marker + "refresh_token", SessionCookie: marker + "session_cookie",
				SessionID: marker + "session_id", ClientUat: marker + "client_uat",
				OAuthAccessToken: marker + "oauth_access_token", OAuthRefreshToken: marker + "oauth_refresh_token",
				WorkBuddyAccessToken: marker + "workbuddy_access_token", WorkBuddyRefreshToken: marker + "workbuddy_refresh_token",
				QoderAccessToken: marker + "qoder_access_token", QoderRefreshToken: marker + "qoder_refresh_token",
				QoderRuntimeInfo: marker + "qoder_runtime_info", QoderRuntimeKey: marker + "qoder_runtime_key",
			}
			for _, key := range exportedCredentialKeys(t, acc) {
				if !allowed[channel][key] {
					t.Errorf("export of a %s account carries %q, which belongs to another channel", channel, key)
				}
			}
		})
	}
}

// TestExportKeepsOAuthCredentialsForAnOAuthAccount pins the one place where the
// export is deliberately more revealing than the account API: an OAuth export that
// dropped its credential could not be re-imported, and there is no other way to
// recreate one.
func TestExportKeepsOAuthCredentialsForAnOAuthAccount(t *testing.T) {
	acc := &store.Account{
		ID: 1, Name: "oauth", AccountType: "grok", CredentialType: "oauth",
		OAuthAccessToken:  marker + "oauth_access_token",
		OAuthRefreshToken: marker + "oauth_refresh_token",
		Enabled:           true, Weight: 1,
	}
	got := exportedCredentialKeys(t, acc)
	for _, want := range []string{"oauth_access_token", "oauth_refresh_token"} {
		if !containsString(got, want) {
			t.Errorf("OAuth export dropped %q; it must stay re-importable (got %v)", want, got)
		}
	}
	// The account API still hides them: the export is the only surface allowed to
	// carry them.
	raw, err := json.Marshal(normalizeAccountOutput(acc))
	if err != nil {
		t.Fatalf("marshal account output: %v", err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatalf("account API exposed an OAuth credential: %s", raw)
	}
}

// TestExportStillSkipsWarp makes sure the new redaction step did not change which
// accounts are exported at all.
func TestExportStillSkipsWarp(t *testing.T) {
	acc := &store.Account{ID: 1, Name: "w", AccountType: "warp", RefreshToken: marker + "refresh_token",
		Enabled: true, Weight: 1}
	if got := exportedCredentialKeys(t, acc); len(got) != 0 {
		t.Fatalf("exported a Warp session: %v", got)
	}
}

func setOf(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestExportCarriesTheDurableCredentialForReimport pins the other half of the
// contract: the export is a portability artifact, so a channel whose only way to
// renew is a rotated refresh token must export it.
//
// Without the refresh token a restored WorkBuddy or Qoder account works until its
// access token expires and then cannot recover — there is no other renewal path —
// which made the export unusable for exactly the channels the file exists to move.
func TestExportCarriesTheDurableCredentialForReimport(t *testing.T) {
	cases := map[string]struct {
		acc  *store.Account
		want []string
	}{
		"workbuddy": {
			acc: &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true, Weight: 1,
				WorkBuddyAccessToken: marker + "access", WorkBuddyRefreshToken: marker + "refresh"},
			want: []string{"workbuddy_access_token", "workbuddy_refresh_token"},
		},
		"qoder": {
			acc: &store.Account{ID: 1, AccountType: "qoder", Enabled: true, Weight: 1,
				QoderAccessToken: marker + "access", QoderRefreshToken: marker + "refresh",
				QoderRuntimeInfo: marker + "info", QoderRuntimeKey: marker + "key"},
			want: []string{"qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key"},
		},
	}
	for channel, tc := range cases {
		t.Run(channel, func(t *testing.T) {
			got := exportedCredentialKeys(t, tc.acc)
			for _, want := range tc.want {
				if !containsString(got, want) {
					t.Errorf("export dropped %q; the account could not be renewed after import (got %v)", want, got)
				}
			}
		})
	}
}
