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
		acc := lb.selectAccountWithTracker(accounts, nil)
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
		acc := lb.selectAccountWithTracker(accounts, nil)
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
		selected := lb.selectAccountWithTracker(accounts, nil)
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
			{ID: 1, Name: "Puter1", AccountType: "puter", Enabled: true, StatusCode: "429", LastAttempt: now},
			{ID: 2, Name: "Puter2", AccountType: "puter", Enabled: true, StatusCode: "429", LastAttempt: now},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "puter", nil)
	if err == nil {
		t.Fatal("expected rate-limited selector error, got nil")
	}
	if !strings.Contains(err.Error(), "all matching accounts are rate-limited or cooling down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetNextAccountExcludingByChannelWithTracker_RejectsSingleAccountAtLimit(t *testing.T) {
	tracker := NewMemoryConnTracker()
	tracker.Acquire(1)
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: tracker,
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "Grok1", AccountType: "grok", Enabled: true, MaxConcurrent: 1},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "grok", tracker)
	if err == nil || !strings.Contains(err.Error(), "concurrency limit") {
		t.Fatalf("expected concurrency limit error, got %v", err)
	}
}

func TestMemoryConnTrackerTryAcquireIsBounded(t *testing.T) {
	tracker := NewMemoryConnTracker()
	if !tracker.TryAcquire(7, 1) {
		t.Fatal("first reservation should succeed")
	}
	if tracker.TryAcquire(7, 1) {
		t.Fatal("second reservation should be rejected")
	}
	tracker.Release(7)
	if !tracker.TryAcquire(7, 1) {
		t.Fatal("reservation should succeed after release")
	}
}

func TestIsAccountAvailable_401RequiresReauth(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", StatusCode: "401", AuthStatus: store.AccountAuthStatusReauthRequired, LastAttempt: time.Now().Add(-24 * time.Hour)}
	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("reauthRequired account must remain excluded regardless of age")
	}
}

func TestIsAccountAvailable_PaidGrokBillingExhaustion(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", GrokProvider: "build", CredentialType: "oauth", Subscription: "super"}
	acc.GrokBilling.Monthly = store.GrokQuotaWindow{HasLimit: true, Limit: 100, HasRemaining: true, Remaining: 0, ResetAt: time.Now().Add(time.Hour)}
	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("known exhausted paid Build account must be gated")
	}
	acc.GrokBilling.Monthly.Remaining = 1
	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("paid Build account with remaining billing must be available")
	}
}

func TestIsAccountAvailable_Paid402UsesBillingPeriodEnd(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", GrokProvider: "build", CredentialType: "oauth", Subscription: "super", StatusCode: "402", LastAttempt: time.Now().Add(-48 * time.Hour)}
	acc.GrokBilling.Weekly = store.GrokQuotaWindow{HasUsage: true, UsagePercent: 100, ResetAt: time.Now().Add(time.Hour)}
	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("paid 402 must remain gated until billing period end")
	}
	acc.GrokBilling.Weekly.ResetAt = time.Now().Add(-time.Second)
	// A post-period probe is admitted through the atomic store claim, not by a
	// store-less LoadBalancer: without the claim every concurrent request would
	// hit the exhausted account at once.
	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("paid 402 must remain gated when no atomic probe store is configured")
	}
}

func TestIsAccountAvailable_429UsesQuotaResetAt(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:           1,
		AccountType:  "warp",
		StatusCode:   "429",
		LastAttempt:  time.Now().Add(-time.Minute),
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

func TestIsAccountAvailable_WarpQuotaExhaustedRemainsAvailableForFiltering(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:                   1,
		AccountType:          "warp",
		Subscription:         "build/business",
		StatusCode:           store.AccountStatusWarpQuotaExhausted,
		LastAttempt:          time.Now(),
		WarpMonthlyLimit:     1500,
		WarpMonthlyRemaining: 0,
	}

	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected quota-exhausted Warp account to remain selectable for free-only filtering")
	}
	if acc.StatusCode != store.AccountStatusWarpQuotaExhausted {
		t.Fatalf("expected durable quota status to remain, got %q", acc.StatusCode)
	}
}

func TestIsAccountAvailable_WarpQuotaStatusClearsAfterQuotaRefresh(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:                   1,
		AccountType:          "warp",
		StatusCode:           store.AccountStatusWarpQuotaExhausted,
		WarpMonthlyLimit:     1500,
		WarpMonthlyRemaining: 100,
	}

	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected refreshed Warp account to be available")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected stale quota status to clear, got %q", acc.StatusCode)
	}
}

func TestIsAccountAvailable_402UsesPuterProbeCooldown(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "puter",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-5 * time.Minute),
	}

	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected Puter 402 account to remain unavailable before probe cooldown expires")
	}

	acc.LastAttempt = time.Now().Add(-(retry402Puter + time.Minute))
	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected expired 402 cooldown to re-enable account")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected status to be cleared after 402 cooldown, got %q", acc.StatusCode)
	}
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionIsHeld pins that an exhausted
// WorkBuddy allowance takes the account out of rotation.
//
// Keeping it in rotation is what turned a spent allowance into a permanent outage:
// every request retried the whole pool, every attempt was refused upstream, and the
// account table showed no reason because a model-scoped verdict writes no status.
// The upstream refuses an exhausted account for every model, so there is no free
// capacity to protect by leaving it in.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionIsHeld(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:            1,
		AccountType:   "workbuddy",
		StatusCode:    "402",
		StatusMessage: "credits exhausted",
		LastAttempt:   time.Now(),
		QuotaResetAt:  time.Now().Add(48 * time.Hour),
	}

	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected a credit-exhausted WorkBuddy account to be out of rotation")
	}
	if acc.StatusCode != "402" {
		t.Fatalf("expected the verdict to stand, got %q", acc.StatusCode)
	}
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionReturnsAtReset is the recovery
// half: the account comes back on its own when the allowance does, without an
// operator having to clear anything.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionReturnsAtReset(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:           1,
		AccountType:  "workbuddy",
		StatusCode:   "402",
		LastAttempt:  time.Now().Add(-time.Hour),
		QuotaResetAt: time.Now().Add(-time.Minute),
	}

	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected the account to return to rotation once its quota reset time passed")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected the spent-allowance marker to be cleared, got %q", acc.StatusCode)
	}
}

// TestIsAccountAvailable_402KeepsLongCooldownForOtherChannels pins that the
// WorkBuddy release above is channel-scoped: every other provider keeps the
// payment cooldown.
func TestIsAccountAvailable_402KeepsLongCooldownForOtherChannels(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "other",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-time.Hour),
	}

	if lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected non-Puter 402 account to keep the long cooldown")
	}
}

func TestMarkAccountStatus_Repeated429RefreshesCooldownStart(t *testing.T) {
	lb := &LoadBalancer{
		Store:       &store.Store{},
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "Puter1", AccountType: "puter", Enabled: true},
		},
	}
	acc := &store.Account{
		ID:          1,
		AccountType: "puter",
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
	if acc.RateLimitFailures != 1 || lb.cachedAccounts[0].RateLimitFailures != 1 {
		t.Fatalf("expected failure count persisted to account/cache: acc=%d cache=%d", acc.RateLimitFailures, lb.cachedAccounts[0].RateLimitFailures)
	}
	remaining := time.Until(acc.QuotaResetAt)
	if remaining < 29*time.Second || remaining > 31*time.Second {
		t.Fatalf("first 429 cooldown=%v want about 30s", remaining)
	}
	lb.MarkAccountStatus(context.Background(), acc, "429")
	remaining = time.Until(acc.QuotaResetAt)
	if acc.RateLimitFailures != 2 || remaining < 59*time.Second || remaining > 61*time.Second {
		t.Fatalf("second 429 failures=%d cooldown=%v want about 1m", acc.RateLimitFailures, remaining)
	}
}
