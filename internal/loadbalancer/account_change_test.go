package loadbalancer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
)

// TestInvalidateAccounts_MakesChangesImmediate is the acceptance rule for the
// notification chain: a change must be visible to the NEXT request, not after the
// cache TTL expires. Before this, a deleted or disabled account could still be
// selected for up to cacheTTL.
func TestInvalidateAccounts_MakesChangesImmediate(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "pool-invalidate:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{AccountType: "warp", RefreshToken: "session-a", Enabled: true, Weight: 1}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// A long TTL makes the point: without notification the pool would keep serving
	// the stale snapshot for the whole window.
	lb := NewWithCacheTTL(s, time.Hour)
	if _, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "warp", nil, nil); err != nil {
		t.Fatalf("first selection: %v", err)
	}

	// Delete the account, then notify as the change bus would.
	if err := s.DeleteAccount(context.Background(), acc.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	lb.AccountChanges([]int64{acc.ID})

	if _, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "warp", nil, nil); err == nil {
		t.Fatal("a deleted account was still selectable after the invalidation")
	}
}

// TestInvalidateAccounts_KeepsUnrelatedAccounts guards the blast radius: only the
// changed account leaves the snapshot.
func TestInvalidateAccounts_KeepsUnrelatedAccounts(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "pool-keep:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	first := &store.Account{AccountType: "warp", RefreshToken: "session-a", Enabled: true, Weight: 1}
	second := &store.Account{AccountType: "warp", RefreshToken: "session-b", Enabled: true, Weight: 1}
	for _, acc := range []*store.Account{first, second} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}

	lb := NewWithCacheTTL(s, time.Hour)
	if _, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "warp", nil, nil); err != nil {
		t.Fatalf("first selection: %v", err)
	}

	lb.AccountChanges([]int64{first.ID})

	lb.mu.RLock()
	remaining := len(lb.cachedAccounts)
	ids := make([]int64, 0, remaining)
	for _, acc := range lb.cachedAccounts {
		ids = append(ids, acc.ID)
	}
	lb.mu.RUnlock()
	if remaining != 1 || ids[0] != second.ID {
		t.Fatalf("snapshot after invalidation = %v, want only the untouched account %d", ids, second.ID)
	}
}

// TestInvalidateAccounts_WithEmptySnapshotIsSafe covers the cold path.
func TestInvalidateAccounts_WithEmptySnapshotIsSafe(t *testing.T) {
	lb := NewWithCacheTTL(nil, time.Minute)
	lb.AccountChanges([]int64{1, 2, 3})
	lb.AccountChanges(nil)
	if len(lb.cachedAccounts) != 0 {
		t.Fatal("invalidating an empty snapshot changed it")
	}
}
