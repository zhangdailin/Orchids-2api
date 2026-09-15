package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestPreserveLatestAccountStatus_PreservesBlockedState(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	original := &store.Account{
		Name:        "warp-1",
		AccountType: "warp",
		Enabled:     true,
		Weight:      1,
		StatusCode:  "403",
		LastAttempt: time.Now().Add(-2 * time.Minute),
	}
	if err := s.CreateAccount(context.Background(), original); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	stale := &store.Account{
		ID:          original.ID,
		Name:        original.Name,
		AccountType: original.AccountType,
		Enabled:     true,
		Weight:      1,
	}

	preserveLatestAccountStatus(context.Background(), s, stale)

	if stale.StatusCode != "403" {
		t.Fatalf("status_code=%q want 403", stale.StatusCode)
	}
	if stale.LastAttempt.IsZero() {
		t.Fatal("expected last_attempt to be preserved")
	}
}

func TestBuildGrokRefreshCandidates_DeduplicatesByToken(t *testing.T) {
	accounts := []*store.Account{
		{ID: 1, AccountType: "grok", Enabled: true, ClientCookie: "sso=shared-token", AgentMode: "grok-4.3"},
		{ID: 2, AccountType: "grok", Enabled: true, ClientCookie: "shared-token"},
		{ID: 3, AccountType: "grok", Enabled: true, RefreshToken: "other-token"},
		{ID: 4, AccountType: "warp", Enabled: true, ClientCookie: "sso=warp-token"},
		{ID: 5, AccountType: "grok", Enabled: true},
	}

	got := buildGrokRefreshCandidates(accounts)
	if len(got) != 2 {
		t.Fatalf("candidate count=%d want 2", len(got))
	}
	if got[0].token != "shared-token" {
		t.Fatalf("first token=%q want shared-token", got[0].token)
	}
	if len(got[0].accounts) != 2 {
		t.Fatalf("shared-token account count=%d want 2", len(got[0].accounts))
	}
	if got[0].model != "grok-4.3" {
		t.Fatalf("shared-token model=%q want grok-4.3", got[0].model)
	}
	if got[1].token != "other-token" {
		t.Fatalf("second token=%q want other-token", got[1].token)
	}
}

func TestBuildGrokRefreshCandidates_ExcludesLinkedConsoleAccounts(t *testing.T) {
	accounts := []*store.Account{
		{ID: 1, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=shared-token"},
		{ID: 2, AccountType: "grok", CredentialType: "sso", GrokProvider: "console", GrokSSOParentID: 1, ClientCookie: "sso=shared-token"},
		{ID: 3, AccountType: "grok", CredentialType: "sso", GrokProvider: "console", ClientCookie: "sso=standalone-console"},
	}

	got := buildGrokRefreshCandidates(accounts)
	if len(got) != 1 {
		t.Fatalf("candidate count=%d want 1", len(got))
	}
	if got[0].token != "shared-token" || len(got[0].accounts) != 1 || got[0].accounts[0].ID != 1 {
		t.Fatalf("Web refresh candidates=%+v want only Web account", got)
	}
}

// TestPlanGrokRefreshCycle_OrdersByDueTimeAndNeverSkipsUnverified replaces the
// old rotation-offset expectations: urgent work is chosen by due time, and an
// account with no verdict is the most urgent because "never checked" is not a
// health state.
func TestPlanGrokRefreshCycle_OrdersByDueTimeAndNeverSkipsUnverified(t *testing.T) {
	now := time.Now()
	fresh := &store.Account{ID: 1, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=a", Enabled: true, VerifiedAt: now.Add(-time.Minute)}
	stale := &store.Account{ID: 2, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=b", Enabled: true, VerifiedAt: now.Add(-4 * time.Hour)}
	never := &store.Account{ID: 3, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=c", Enabled: true}

	candidates := buildGrokRefreshCandidates([]*store.Account{fresh, stale, never})
	planned := planGrokRefreshCycle(candidates)
	if len(planned) != 2 {
		t.Fatalf("planned = %d, want only overdue and never-verified accounts", len(planned))
	}
	if firstCandidateAccount(planned[0]).ID != 3 {
		t.Fatalf("first plan = %d, want the never-verified account (id 3)", firstCandidateAccount(planned[0]).ID)
	}
	if firstCandidateAccount(planned[1]).ID != 2 {
		t.Fatalf("second plan = %d, want the most overdue verified account (id 2)", firstCandidateAccount(planned[1]).ID)
	}
}

func TestProviderHealthRefreshDueUsesSlowCadence(t *testing.T) {
	now := time.Now()
	if providerHealthRefreshDue(&store.Account{VerifiedAt: now.Add(-time.Minute)}, now) {
		t.Fatal("fresh healthy account was scheduled on credential tick")
	}
	if !providerHealthRefreshDue(&store.Account{VerifiedAt: now.Add(-providerHealthRefreshInterval - time.Minute)}, now) {
		t.Fatal("stale healthy account was not scheduled")
	}
	if !providerHealthRefreshDue(&store.Account{}, now) {
		t.Fatal("never-verified account was not scheduled immediately")
	}
}

func TestRefreshQoderQuotaClearsFalseAgentExhaustion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userType":"personal_pro","userQuota":{"total":300,"used":82,"remaining":218,"unit":"credits"}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"user_type":"personal_pro","plan_tier_name":"Pro","is_paid_plan":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "qoder-auto-quota:"})
	if err != nil {
		t.Fatalf("store.New() error=%v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{
		AccountType: "qoder", Enabled: true, StatusCode: "402",
		StatusMessage: "agent limit", LastAttempt: time.Now().Add(-time.Hour),
		QoderAccessToken: "access", QoderRefreshToken: "refresh",
		QoderExpiresAt: time.Now().Add(6 * time.Hour), QoderMachineID: "machine",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error=%v", err)
	}
	cfg := &config.Config{QoderOpenAPIBaseURL: upstream.URL, QoderOAuthBaseURL: upstream.URL, QoderInferenceURL: upstream.URL}
	refreshQoderQuota(context.Background(), cfg, s, acc)

	after, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error=%v", err)
	}
	if after.StatusCode != "" || after.QoderQuota.Exhausted {
		t.Fatalf("false exhaustion survived authoritative sync: status=%q quota=%+v", after.StatusCode, after.QoderQuota)
	}
	if after.QoderQuota.Remaining != 218 || after.UsageCurrent != 218 {
		t.Fatalf("remaining=%v usage_current=%v want 218", after.QoderQuota.Remaining, after.UsageCurrent)
	}
}

func TestGrokCLIBillingNeedsSyncUsesSlowCadence(t *testing.T) {
	now := time.Now()
	if grokCLIBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Minute)}}, now) {
		t.Fatal("fresh CLI billing was polled on credential tick")
	}
	if !grokCLIBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Hour)}}, now) {
		t.Fatal("stale CLI billing was not polled")
	}
}

// TestPlanGrokRefreshCycle_SkipsAccountHoldingALease covers the write-back
// guard: an account already refreshing is merged, never scheduled twice.
func TestPlanGrokRefreshCycle_SkipsAccountHoldingALease(t *testing.T) {
	now := time.Now()
	busy := &store.Account{ID: 11, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=x", Enabled: true, VerifiedAt: now.Add(-5 * time.Hour)}
	idle := &store.Account{ID: 12, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=y", Enabled: true, VerifiedAt: now.Add(-5 * time.Hour)}

	grokRefreshHub.TryAcquire(11)
	t.Cleanup(func() { grokRefreshHub.Release(11) })

	planned := planGrokRefreshCycle(buildGrokRefreshCandidates([]*store.Account{busy, idle}))
	for _, candidate := range planned {
		if firstCandidateAccount(candidate).ID == 11 {
			t.Fatal("an account holding a lease must not be scheduled")
		}
	}
	if len(planned) != 1 {
		t.Fatalf("planned = %d, want only the free account", len(planned))
	}
}
