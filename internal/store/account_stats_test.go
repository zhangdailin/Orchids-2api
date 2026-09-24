package store

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestIncrementAccountStats_PassthroughAccountKeepsRemoteQuotaCurrent(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	ctx := context.Background()
	acc := &Account{
		AccountType:  "warp",
		Enabled:      true,
		UsageCurrent: 11_000_000,
		UsageLimit:   11_000_000,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if err := s.IncrementAccountStats(ctx, acc.ID, 2048, 1); err != nil {
		t.Fatalf("IncrementAccountStats() error = %v", err)
	}

	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.UsageCurrent != 11_000_000 {
		t.Fatalf("usage_current=%v want 11000000", got.UsageCurrent)
	}
	if got.UsageTotal != 2048 {
		t.Fatalf("usage_total=%v want 2048", got.UsageTotal)
	}
	if got.RequestCount != 1 {
		t.Fatalf("request_count=%d want 1", got.RequestCount)
	}
}

func TestIncrementAccountStats_ZeroUsageStillCountsRequest(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	ctx := context.Background()
	acc := &Account{
		AccountType:  "warp",
		Enabled:      true,
		UsageCurrent: 11_000_000,
		UsageTotal:   123,
		UsageLimit:   11_000_000,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	if err := s.IncrementAccountStats(ctx, acc.ID, 0, 1); err != nil {
		t.Fatalf("IncrementAccountStats() error = %v", err)
	}

	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.UsageCurrent != 11_000_000 {
		t.Fatalf("usage_current=%v want 11000000", got.UsageCurrent)
	}
	if got.UsageTotal != 123 {
		t.Fatalf("usage_total=%v want 123", got.UsageTotal)
	}
	if got.RequestCount != 1 {
		t.Fatalf("request_count=%d want 1", got.RequestCount)
	}
}

func TestUpdateAccount_DoesNotOverwriteAtomicUsageCounters(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	stale, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if err := s.IncrementAccountStats(ctx, acc.ID, 100, 1); err != nil {
		t.Fatalf("IncrementAccountStats() error = %v", err)
	}
	stale.StatusCode = "429"
	if err := s.UpdateAccount(ctx, stale); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.UsageTotal != 100 || got.TokensToday != 100 || got.TokensDate == "" {
		t.Fatalf("atomic counters overwritten by stale update: total=%v today=%v date=%q", got.UsageTotal, got.TokensToday, got.TokensDate)
	}
}

func TestIncrementAccountStats_WorkBuddyKeepsRemoteRemainingCredits(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &Account{AccountType: "workbuddy", Enabled: true, UsageCurrent: 0, UsageLimit: 1000}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := s.IncrementAccountStats(ctx, acc.ID, 500, 1); err != nil {
		t.Fatalf("IncrementAccountStats() error = %v", err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.UsageCurrent != 0 || got.UsageTotal != 500 || got.TokensToday != 500 {
		t.Fatalf("workbuddy counters = current %v total %v today %v, want 0/500/500", got.UsageCurrent, got.UsageTotal, got.TokensToday)
	}
}

func TestUpdateAccount_PersistsWarpQuotaBreakdown(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	ctx := context.Background()
	acc := &Account{
		AccountType:          "warp",
		Enabled:              true,
		RefreshToken:         "rt",
		UsageCurrent:         1429,
		UsageLimit:           1550,
		WarpMonthlyLimit:     1550,
		WarpMonthlyRemaining: 121,
		WarpBonusRemaining:   1000,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	acc.WarpMonthlyRemaining = 120
	acc.WarpBonusRemaining = 999
	if err := s.UpdateAccount(ctx, acc); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.WarpMonthlyLimit != 1550 {
		t.Fatalf("warp_monthly_limit=%v want 1550", got.WarpMonthlyLimit)
	}
	if got.WarpMonthlyRemaining != 120 {
		t.Fatalf("warp_monthly_remaining=%v want 120", got.WarpMonthlyRemaining)
	}
	if got.WarpBonusRemaining != 999 {
		t.Fatalf("warp_bonus_remaining=%v want 999", got.WarpBonusRemaining)
	}
}
