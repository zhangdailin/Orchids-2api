package loadbalancer

import (
	"context"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

type fixedConnTracker struct {
	counts map[int64]int64
}

func (t *fixedConnTracker) Acquire(accountID int64) {}

func (t *fixedConnTracker) Release(accountID int64) {}

func (t *fixedConnTracker) GetCount(accountID int64) int64 {
	if t == nil {
		return 0
	}
	return t.counts[accountID]
}

func (t *fixedConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	out := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = t.GetCount(id)
	}
	return out
}

func TestSelectAccount_Distribution(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	accounts := []*store.Account{
		{ID: 1, Name: "Acc1", Weight: 1},
		{ID: 2, Name: "Acc2", Weight: 1},
		{ID: 3, Name: "Acc3", Weight: 1},
	}

	counts := make(map[int64]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		acc := lb.selectAccount(accounts)
		if acc == nil {
			t.Fatal("selectAccount returned nil")
		}
		counts[acc.ID]++
	}

	if len(counts) < 2 {
		t.Errorf("Expected distribution across multiple accounts, but only got %d accounts", len(counts))
	}

	t.Logf("Counts after %d iterations: %+v", iterations, counts)

	// Ensure each account got a reasonable number of hits (rough check)
	for id, count := range counts {
		if count < 200 {
			t.Errorf("Account %d got suspiciously low hits: %d", id, count)
		}
	}
}

func TestSelectAccount_WeightedDistribution(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	// Acc1 has weight 10, Acc2 has weight 1
	// With 0 active conns, the score for both is 0/10 = 0 and 0/1 = 0.
	// So they should still be tied and picked randomly.
	accounts := []*store.Account{
		{ID: 1, Name: "Acc1", Weight: 10},
		{ID: 2, Name: "Acc2", Weight: 1},
	}

	counts := make(map[int64]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		acc := lb.selectAccount(accounts)
		counts[acc.ID]++
	}

	if counts[1] == 0 || counts[2] == 0 {
		t.Errorf("Expected both accounts to be picked when tied at score 0, got counts: %+v", counts)
	}
}

func TestSelectAccount_ActiveConnections(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc1 := &store.Account{ID: 1, Name: "Acc1", Weight: 1}
	acc2 := &store.Account{ID: 2, Name: "Acc2", Weight: 1}
	accounts := []*store.Account{acc1, acc2}

	// Mock active connections
	lb.AcquireConnection(acc1.ID) // acc1 has 1 conn, score 1/1 = 1
	// acc2 has 0 conns, score 0/1 = 0

	// Should always pick acc2
	for i := 0; i < 100; i++ {
		selected := lb.selectAccount(accounts)
		if selected.ID != acc2.ID {
			t.Errorf("Expected Acc2 to be selected, got %s", selected.Name)
		}
	}
}

func TestSelectAccountWithTracker_UsesProvidedTracker(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc1 := &store.Account{ID: 1, Name: "Acc1", Weight: 1}
	acc2 := &store.Account{ID: 2, Name: "Acc2", Weight: 1}
	accounts := []*store.Account{acc1, acc2}

	custom := &fixedConnTracker{
		counts: map[int64]int64{
			acc1.ID: 5,
			acc2.ID: 0,
		},
	}

	for i := 0; i < 100; i++ {
		selected := lb.selectAccountWithTracker(accounts, custom)
		if selected == nil || selected.ID != acc2.ID {
			t.Fatalf("expected Acc2 to be selected via custom tracker, got %#v", selected)
		}
	}
}

func TestGetNextAccountExcludingByChannelWithTracker_AllRateLimitedReturnsHelpfulError(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "Bolt1", AccountType: "bolt", Enabled: true, StatusCode: "429", LastAttempt: now},
			{ID: 2, Name: "Bolt2", AccountType: "bolt", Enabled: true, StatusCode: "429", LastAttempt: now},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "bolt", nil)
	if err == nil {
		t.Fatal("expected rate-limited selector error, got nil")
	}
	if !strings.Contains(err.Error(), "all matching accounts are rate-limited or cooling down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsAccountAvailable_429UsesQuotaResetAt(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:           1,
		AccountType:  "bolt",
		StatusCode:   "429",
		LastAttempt:  time.Now(),
		QuotaResetAt: time.Now().Add(-time.Second),
	}

	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected expired quota reset to re-enable account")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected status to be cleared after cooldown, got %q", acc.StatusCode)
	}
	if !acc.QuotaResetAt.IsZero() {
		t.Fatalf("expected quota reset timestamp to be cleared, got %v", acc.QuotaResetAt)
	}
}

func TestIsAccountAvailable_402UsesLongCooldown(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "puter",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-2 * time.Hour),
	}

	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected 402 account to remain unavailable before long cooldown expires")
	}

	acc.LastAttempt = time.Now().Add(-(retry402Default + time.Minute))
	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected expired 402 cooldown to re-enable account")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected status to be cleared after 402 cooldown, got %q", acc.StatusCode)
	}
}

func TestMarkAccountStatus_Repeated429RefreshesCooldownStart(t *testing.T) {
	lb := &LoadBalancer{
		Store:       &store.Store{},
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "Bolt1", AccountType: "bolt", Enabled: true},
		},
	}
	acc := &store.Account{
		ID:          1,
		AccountType: "bolt",
		StatusCode:  "429",
		LastAttempt: time.Now().Add(-30 * time.Second),
	}

	before := acc.LastAttempt
	lb.MarkAccountStatus(context.Background(), acc, "429")

	if !acc.LastAttempt.After(before) {
		t.Fatalf("expected repeated 429 to refresh cooldown start, before=%v after=%v", before, acc.LastAttempt)
	}
	if got := lb.cachedAccounts[0].LastAttempt; !got.After(before) {
		t.Fatalf("expected cached repeated 429 to refresh cooldown start, before=%v after=%v", before, got)
	}
}
