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

// TestGetNextAccountExcludingByChannelWithTracker_AllAllowanceParkedNamesTheAllowance
// pins the second reason an empty pool has: every account is parked by an
// exhausted allowance with its reset time still ahead. The caller has to be able
// to tell that apart from a rate limit, because the action differs — credits and
// capacity, rather than waiting out a cooldown.
func TestGetNextAccountExcludingByChannelWithTracker_AllAllowanceParkedNamesTheAllowance(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 2, Name: "WB2", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "workbuddy", nil)
	if err == nil {
		t.Fatal("expected an allowance-parked selector error, got nil")
	}
	if !strings.Contains(err.Error(), "have exhausted their allowance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestGetNextAccountExcludingByChannelWithTrackerFilter_ModelFilterEmptiesThePool
// pins the reason that produced the WorkBuddy outage this was written for: the
// channel has accounts, but every one of them is cooling down for the model the
// request asked for. The bare "no enabled accounts available for channel" made
// that read like a channel with no accounts at all.
func TestGetNextAccountExcludingByChannelWithTrackerFilter_ModelFilterEmptiesThePool(t *testing.T) {
	now := time.Now()
	tracker := NewMemoryConnTracker()
	lb := &LoadBalancer{
		connTracker: tracker,
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", tracker, func(*store.Account) bool {
		// The per-model cooldown filter: every candidate is withheld for this model.
		return false
	})
	if err == nil {
		t.Fatal("expected a model-filtered selector error, got nil")
	}
	if !strings.Contains(err.Error(), "cooling down for the requested model") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestGetNextAccountExcludingByChannelWithTracker_MixedPoolNamesTheSplit is the
// regression test for the 2026-09-21 WorkBuddy outage.
//
// The pool held three accounts cooling down from a 429 and four parked for a
// spent allowance at the same time. Both group-only rules require *every*
// account to share one reason, so neither matched, the selector fell through to
// the bare "no enabled accounts available for channel: workbuddy", and that
// sentence classifies to no capacity cause at all — the caller was answered with
// a 503 "server fault" instead of a retryable 429, and the operator could not
// tell rate limits from spent credits.
func TestGetNextAccountExcludingByChannelWithTracker_MixedPoolNamesTheSplit(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 2, Name: "WB2", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 3, Name: "WB3", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 4, Name: "WB4", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 5, Name: "WB5", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 6, Name: "WB6", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 7, Name: "WB7", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "workbuddy", nil)
	if err == nil {
		t.Fatal("expected a mixed-pool selector error, got nil")
	}
	message := err.Error()
	if !strings.Contains(message, "rate-limited or cooling down") {
		t.Fatalf("mixed pool must name the rate limit: %v", err)
	}
	if !strings.Contains(message, "3 rate-limited") || !strings.Contains(message, "4 parked for a spent allowance") {
		t.Fatalf("mixed pool must report both counts: %v", err)
	}
	// The phrase "exhausted their allowance" would make the shared pool rule
	// classify a recoverable mixed pool as a permanent quota verdict.
	if strings.Contains(message, "exhausted their allowance") {
		t.Fatalf("mixed pool must not claim every account is out of quota: %v", err)
	}
	// And it must not degrade to the bare sentence that answered 503.
	if strings.HasSuffix(message, "channel: workbuddy") {
		t.Fatalf("mixed pool fell back to the unexplained selector sentence: %v", err)
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

func TestIsAccountAvailable_LegacyPuter402ReachesModelFilter(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "puter",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-5 * time.Minute),
	}

	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected legacy Puter 402 account to reach the free-model filter")
	}
	if acc.StatusCode != "402" {
		t.Fatalf("expected legacy quota marker preserved, got %q", acc.StatusCode)
	}
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionReachesModelFilter pins that
// the dedicated spent-package state remains a candidate; the handler's
// model-aware filter then admits only confirmed advertised free models.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionReachesModelFilter(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID: 1, AccountType: "workbuddy", StatusCode: store.AccountStatusWorkBuddyQuotaExhausted,
		StatusMessage: "credits exhausted", LastAttempt: time.Now(), QuotaResetAt: time.Now().Add(48 * time.Hour),
	}
	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected exhausted WorkBuddy account to reach the free-model filter")
	}
	if acc.StatusCode != store.AccountStatusWorkBuddyQuotaExhausted {
		t.Fatalf("expected free-only state preserved, got %q", acc.StatusCode)
	}
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionClearsWhenQuotaReturns proves
// that positive quota restores full account capability.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionClearsWhenQuotaReturns(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "workbuddy", StatusCode: store.AccountStatusWorkBuddyQuotaExhausted, UsageCurrent: 10}
	if !lb.isAccountAvailable(context.Background(), acc) {
		t.Fatal("expected account to remain available when quota returns")
	}
	if acc.StatusCode != "" {
		t.Fatalf("expected free-only marker cleared, got %q", acc.StatusCode)
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

func TestSelectAccountRotatesAcrossEqualAccounts(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	accounts := []*store.Account{
		{ID: 1, Name: "a", Weight: 1},
		{ID: 2, Name: "b", Weight: 1},
		{ID: 3, Name: "c", Weight: 1},
	}
	seen := map[int64]int{}
	for i := 0; i < 30; i++ {
		acc := lb.selectAccountWithTracker(accounts, nil)
		if acc == nil {
			t.Fatal("nil account")
		}
		seen[acc.ID]++
	}
	if len(seen) != 3 {
		t.Fatalf("selection hit only %d accounts; equally loaded accounts must rotate", len(seen))
	}
	for id, count := range seen {
		if count < 5 {
			t.Fatalf("account %d selected %d times out of 30; rotation is too uneven", id, count)
		}
	}
}

func TestLargePoolIsScannedInRotatingWindows(t *testing.T) {
	if accountScanWindow < 8 {
		t.Fatalf("window %d is too small to be useful", accountScanWindow)
	}
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	size := accountScanWindow * 3
	seen := map[int]bool{}
	for i := 0; i < size; i++ {
		start := lb.rotateScanCursor(size)
		for offset := 0; offset < accountScanWindow; offset++ {
			seen[(start+offset)%size] = true
		}
	}
	if len(seen) != size {
		t.Fatalf("rotating windows covered %d of %d accounts", len(seen), size)
	}
}
