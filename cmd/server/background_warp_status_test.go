package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

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

func TestNextGrokRefreshBatch_RotatesAndCapsBatch(t *testing.T) {
	grokRefreshMu.Lock()
	oldOffset := grokRefreshOffset
	grokRefreshOffset = 0
	grokRefreshMu.Unlock()
	t.Cleanup(func() {
		grokRefreshMu.Lock()
		grokRefreshOffset = oldOffset
		grokRefreshMu.Unlock()
	})

	candidates := []grokRefreshCandidate{
		{token: "a"},
		{token: "b"},
		{token: "c"},
		{token: "d"},
	}

	first := nextGrokRefreshBatch(candidates, 2)
	if len(first) != 2 || first[0].token != "a" || first[1].token != "b" {
		t.Fatalf("first batch=%+v want a,b", first)
	}
	second := nextGrokRefreshBatch(candidates, 2)
	if len(second) != 2 || second[0].token != "c" || second[1].token != "d" {
		t.Fatalf("second batch=%+v want c,d", second)
	}
	third := nextGrokRefreshBatch(candidates, 3)
	if len(third) != 3 || third[0].token != "a" || third[1].token != "b" || third[2].token != "c" {
		t.Fatalf("third batch=%+v want a,b,c", third)
	}
}
