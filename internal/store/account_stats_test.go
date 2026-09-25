package store

import (
	"context"
	"testing"
	"time"

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
		AccountType:  "workbuddy",
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
		AccountType:  "workbuddy",
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

func TestIncrementAccountStatsOperationIsDurablyIdempotent(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "stats-idempotent:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	completed := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := s.IncrementAccountStatsOperation(ctx, acc.ID, 42, 1, "request-123", completed); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageTotal != 42 || got.RequestCount != 1 || got.TokensToday != 42 || got.TokensDate != "2026-03-05" {
		t.Fatalf("duplicate operation applied twice: total=%v requests=%d today=%v date=%q", got.UsageTotal, got.RequestCount, got.TokensToday, got.TokensDate)
	}
}

func TestIncrementAccountStatsOperationUsesCompletionUTCDateAcrossMidnight(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "stats-midnight:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	beforeMidnight := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Second)
	if err := s.IncrementAccountStatsOperation(ctx, acc.ID, 10, 1, "newer", afterMidnight); err != nil {
		t.Fatal(err)
	}
	if err := s.IncrementAccountStatsOperation(ctx, acc.ID, 20, 1, "delayed-older", beforeMidnight); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageTotal != 30 || got.RequestCount != 2 {
		t.Fatalf("lifetime totals lost: total=%v requests=%d", got.UsageTotal, got.RequestCount)
	}
	if got.TokensDate != "2026-03-06" || got.TokensToday != 10 {
		t.Fatalf("delayed prior-day completion rewound current UTC bucket: today=%v date=%q", got.TokensToday, got.TokensDate)
	}
}
