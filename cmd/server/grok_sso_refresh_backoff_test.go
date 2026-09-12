package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// TestRefreshGrokAccounts_RetriesSingleRejection keeps the background scheduler
// honest about the same flapping the account page showed: one "unauthenticated"
// answer must not quarantine an account whose cookie the retry accepts.
func TestRefreshGrokAccounts_RetriesSingleRejection(t *testing.T) {
	oldDelay := grokSSORefreshRetryDelay
	grokSSORefreshRetryDelay = time.Millisecond
	t.Cleanup(func() { grokSSORefreshRetryDelay = oldDelay })

	var sessionCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/auth/session":
			if atomic.AddInt32(&sessionCalls, 1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status":"unauthenticated"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"authenticated","session":{"userId":"user-ok","email":"ok@example.com","organizationId":"team-ok"}}`))
		case "/rest/rate-limits":
			_, _ = w.Write([]byte(`{"remainingQueries":100,"totalQueries":100,"remainingTokens":1000,"totalTokens":1000,"windowSizeSeconds":3600}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "sso-retry:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &store.Account{Name: "flaky", AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=token-flaky", Enabled: true, Weight: 1}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	refreshGrokAccounts(ctx, &config.Config{GrokAPIBaseURL: upstream.URL}, s, []*store.Account{acc})

	after, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.StatusCode != "" || after.StatusMessage != "" {
		t.Fatalf("account quarantined %q / %q after a successful retry", after.StatusCode, after.StatusMessage)
	}
	if after.VerifiedAt.IsZero() {
		t.Fatal("a successful sync did not stamp the verdict")
	}
	if after.UserID != "user-ok" {
		t.Fatalf("identity not applied: user=%q", after.UserID)
	}
	if got := atomic.LoadInt32(&sessionCalls); got < 2 {
		t.Fatalf("session calls = %d, want the rejection re-asked", got)
	}
}

// TestGrokRefreshCandidates_SkipDeadCredentials pins the rotation invariant: a
// credential the upstream already rejected must not consume a slot of the
// per-cycle budget on every tick. It stays queued again once the operator
// installs a new cookie (which resets LastAttempt).
func TestGrokRefreshCandidates_SkipDeadCredentials(t *testing.T) {
	now := time.Now()
	accounts := []*store.Account{
		// Rejected 5 minutes ago: still quarantined.
		{ID: 1, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=dead", Enabled: true,
			StatusCode: "401", LastAttempt: now.Add(-5 * time.Minute), VerifiedAt: now.Add(-5 * time.Minute)},
		// Rejected longer than the backoff window: eligible again.
		{ID: 2, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=old-dead", Enabled: true,
			StatusCode: "401", LastAttempt: now.Add(-2 * time.Hour), VerifiedAt: now.Add(-2 * time.Hour)},
		// A fresh credential: the verdict stamp is cleared on credential replacement.
		{ID: 3, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=fresh", Enabled: true},
		// A never-synced account must always be verified: an empty status is not a
		// verdict, and this is exactly the account an operator just added.
		{ID: 4, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=brand-new", Enabled: true},
		// Throttled accounts are not dead; they still need their quota refreshed.
		{ID: 5, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=throttled", Enabled: true,
			StatusCode: "429", LastAttempt: now, VerifiedAt: now},
		// Healthy and already verified: still refreshed on the normal rotation.
		{ID: 6, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=healthy", Enabled: true,
			VerifiedAt: now.Add(-time.Hour)},
	}

	candidates := buildGrokRefreshCandidates(accounts)
	got := map[int64]bool{}
	for _, candidate := range candidates {
		for _, acc := range candidate.accounts {
			got[acc.ID] = true
		}
	}
	for _, id := range []int64{2, 3, 4, 5, 6} {
		if !got[id] {
			t.Fatalf("account %d was skipped from the refresh rotation (queued: %v)", id, got)
		}
	}
	if got[1] {
		t.Fatal("a credential rejected 5 minutes ago is still queued every tick")
	}
}

// TestGrokRefreshDeadCredential_Window covers the helper boundaries.
func TestGrokRefreshDeadCredential_Window(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  *store.Account
		want bool
	}{
		{"nil account", nil, false},
		{"clean account", &store.Account{StatusCode: ""}, false},
		{"401 without a verdict stamp", &store.Account{StatusCode: "401"}, false},
		{"401 inside window", &store.Account{StatusCode: "401", VerifiedAt: now.Add(-time.Minute)}, true},
		{"401 outside window", &store.Account{StatusCode: "401", VerifiedAt: now.Add(-grokRefreshDeadCredentialBackoff - time.Minute)}, false},
		{"429 is not a dead credential", &store.Account{StatusCode: "429", VerifiedAt: now}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokRefreshDeadCredential(tc.acc, now); got != tc.want {
				t.Fatalf("grokRefreshDeadCredential() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsUnverifiedGrokSSOAccount pins the selection that gives a newly added
// account a verdict without waiting for the admin page to run its client-side
// auto-sync. A freshly added row is the report that started this: the second SSO
// account showed fine while the first one displayed the real 401.
func TestIsUnverifiedGrokSSOAccount(t *testing.T) {
	now := time.Now()
	base := &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t"}

	cases := []struct {
		name string
		acc  *store.Account
		want bool
	}{
		{"nil", nil, false},
		{"brand new sso row", &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t"}, true},
		{"already verified", &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t", VerifiedAt: now}, false},
		{"verified but quota-recovered", &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t", VerifiedAt: now, LastAttempt: time.Time{}}, false},
		{"already failing", &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t", StatusCode: "401", VerifiedAt: now}, false},
		{"oauth account", &store.Account{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "a"}, false},
		{"console view", &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: "console", ClientCookie: "sso=t", GrokSSOParentID: 5}, false},
		{"other channel", &store.Account{AccountType: "warp"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnverifiedGrokSSOAccount(tc.acc); got != tc.want {
				t.Fatalf("isUnverifiedGrokSSOAccount() = %v, want %v", got, tc.want)
			}
		})
	}

	// The base row is the canonical case.
	if !isUnverifiedGrokSSOAccount(base) {
		t.Fatal("a fresh SSO row must be scheduled for a first verdict")
	}
}
