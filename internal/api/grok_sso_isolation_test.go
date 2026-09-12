package api

import (
	"context"
	"testing"

	"orchids-api/internal/store"
)

// TestEnsureGrokSSOProviderViews_LeavesOAuthAccountsAlone pins the invariant that
// matters for "adding an OAuth account breaks an existing one": the SSO pairing
// reconciler must never touch Build OAuth rows, whatever their credential marker
// looks like.
func TestEnsureGrokSSOProviderViews_LeavesOAuthAccountsAlone(t *testing.T) {
	s, _ := newTestStore(t, "grok-views:")
	ctx := context.Background()

	oauthOne := &store.Account{
		Name:              "oauth-one",
		AccountType:       "grok",
		CredentialType:    "oauth",
		GrokProvider:      "build",
		OAuthAccessToken:  "access-one",
		OAuthRefreshToken: "refresh-one",
		Enabled:           true,
		Weight:            1,
	}
	ssoWeb := &store.Account{
		Name:           "sso-web",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=web-token",
		Enabled:        true,
		Weight:         1,
	}
	// A legacy OAuth row that never got an explicit credential marker.
	legacyOAuth := &store.Account{
		Name:              "legacy-oauth",
		AccountType:       "grok",
		OAuthAccessToken:  "access-legacy",
		OAuthRefreshToken: "refresh-legacy",
		Enabled:           true,
		Weight:            1,
	}
	for _, acc := range []*store.Account{oauthOne, ssoWeb, legacyOAuth} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}
	before := map[string]*store.Account{}
	for _, acc := range []*store.Account{oauthOne, ssoWeb, legacyOAuth} {
		current, err := s.GetAccount(ctx, acc.ID)
		if err != nil {
			t.Fatalf("GetAccount(%s) error = %v", acc.Name, err)
		}
		before[acc.Name] = current
	}

	a := New(s, "", "", nil)
	if err := a.EnsureGrokSSOProviderViews(ctx); err != nil {
		t.Fatalf("EnsureGrokSSOProviderViews() error = %v", err)
	}

	after := func(name string) *store.Account {
		accounts, err := s.ListAccounts(ctx)
		if err != nil {
			t.Fatalf("ListAccounts() error = %v", err)
		}
		for _, acc := range accounts {
			if acc.Name == name {
				return acc
			}
		}
		return nil
	}

	// Build OAuth rows must survive untouched.
	for _, name := range []string{"oauth-one", "legacy-oauth"} {
		got := after(name)
		if got == nil {
			t.Fatalf("%s disappeared during SSO provider reconciliation", name)
		}
		want := before[name]
		if got.OAuthRefreshToken != want.OAuthRefreshToken || got.OAuthAccessToken != want.OAuthAccessToken {
			t.Fatalf("%s credentials changed: before=%+v after=%+v", name, want, got)
		}
		if !got.Enabled {
			t.Fatalf("%s was disabled by SSO reconciliation", name)
		}
		if got.GrokSSOParentID != 0 {
			t.Fatalf("%s was reparented: parent=%d", name, got.GrokSSOParentID)
		}
	}

	// The Web SSO source keeps working and gains exactly one Console companion.
	web := after("sso-web")
	if web == nil {
		t.Fatal("the Web SSO source disappeared")
	}
	if web.ClientCookie != before["sso-web"].ClientCookie {
		t.Fatalf("the Web SSO credential changed: %q", web.ClientCookie)
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts() error = %v", err)
	}
	companions := 0
	for _, acc := range accounts {
		if acc.GrokSSOParentID == web.ID {
			companions++
		}
	}
	if companions != 1 {
		t.Fatalf("console companions = %d, want exactly 1", companions)
	}
}
