package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// TestGrokRefreshCandidates_DoNotGroupDistinctSSOAccounts pins the invariant that
// motivates the grouping at all: accounts are batched ONLY when they share the
// same credential. The batch is also the unit a 401 is written to, so a wrong
// grouping would mark unrelated accounts as unauthorised.
func TestGrokRefreshCandidates_DoNotGroupDistinctSSOAccounts(t *testing.T) {
	t.Parallel()

	accounts := []*store.Account{
		{ID: 1, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-one", Enabled: true},
		{ID: 2, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-two", Enabled: true},
		{ID: 3, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-one", Enabled: true},
		{ID: 4, AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "a", OAuthRefreshToken: "r", Enabled: true},
	}

	candidates := buildGrokRefreshCandidates(accounts)
	if len(candidates) != 2 {
		for _, candidate := range candidates {
			ids := make([]int64, 0, len(candidate.accounts))
			for _, acc := range candidate.accounts {
				ids = append(ids, acc.ID)
			}
			t.Logf("candidate token=%q ids=%v", candidate.token, ids)
		}
		t.Fatalf("candidates = %d, want 2 (one per distinct SSO credential; OAuth excluded)", len(candidates))
	}

	byToken := map[string][]int64{}
	for _, candidate := range candidates {
		for _, acc := range candidate.accounts {
			byToken[candidate.token] = append(byToken[candidate.token], acc.ID)
		}
	}
	if got := byToken["token-one"]; len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("token-one group = %v, want the two accounts sharing that cookie", got)
	}
	if got := byToken["token-two"]; len(got) != 1 || got[0] != 2 {
		t.Fatalf("token-two group = %v, want only account 2", got)
	}
}

// TestRefreshGrokAccounts_DoesNotCrossMarkSiblingSSOAccounts covers the reported
// behaviour: adding a second SSO account must not flip an unrelated account to
// 401. The stubbed session endpoint rejects the NEW account only; the existing one
// must keep its clean state.
func TestRefreshGrokAccounts_DoesNotCrossMarkSiblingSSOAccounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/auth/session" {
			if strings.Contains(r.Header.Get("Cookie"), "token-two") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-one","email":"one@example.com","organizationId":"team-one"}}`))
			return
		}
		// Quota/model endpoints are irrelevant here.
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "sso-mark:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	first := &store.Account{Name: "first", AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-one", Enabled: true, Weight: 1}
	second := &store.Account{Name: "second", AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-two", Enabled: true, Weight: 1}
	for _, acc := range []*store.Account{first, second} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}

	cfg := &config.Config{GrokAPIBaseURL: upstream.URL}
	refreshGrokAccounts(ctx, cfg, s, []*store.Account{first, second})

	afterFirst, err := s.GetAccount(ctx, first.ID)
	if err != nil {
		t.Fatalf("the first SSO account disappeared: %v", err)
	}
	afterSecond, err := s.GetAccount(ctx, second.ID)
	if err != nil {
		t.Fatalf("the second SSO account disappeared: %v", err)
	}

	if afterFirst.StatusCode != "" {
		t.Fatalf("the valid SSO account was marked %q (%s) by the sibling's 401",
			afterFirst.StatusCode, afterFirst.StatusMessage)
	}
	if !afterFirst.Enabled {
		t.Fatal("the valid SSO account was disabled by the sibling's 401")
	}
	if afterSecond.StatusCode != "401" {
		t.Fatalf("the rejected SSO account status = %q, want 401", afterSecond.StatusCode)
	}
	if afterSecond.StatusMessage == "" {
		t.Fatal("the rejected SSO account has no explanatory message")
	}
}
