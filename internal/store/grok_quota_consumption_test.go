package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newQuotaTestStore(t *testing.T) *Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "quota-test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); mini.Close() })
	return s
}

func TestClaimGrokPaidQuotaProbeIsBoundedAndAtomic(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	acc := &Account{AccountType: "grok", Enabled: true, GrokProvider: "build", Subscription: "super",
		GrokBilling: GrokBillingSnapshot{SyncedAt: now.Add(-time.Hour), Monthly: GrokQuotaWindow{HasLimit: true, Limit: 100, HasRemaining: true, Remaining: 0, ResetAt: now.Add(-time.Second)}}}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	claims := make(chan bool, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			claims <- claimed
		}()
	}
	wg.Wait()
	close(claims)
	count := 0
	for claimed := range claims {
		if claimed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claims=%d want 1", count)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GrokBilling.LastProbeAt.Equal(now) || !got.GrokBilling.NextProbeAt.Equal(now.Add(GrokPaidQuotaProbeInterval)) {
		t.Fatalf("probe schedule=%+v", got.GrokBilling)
	}
	if claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now.Add(time.Minute)); err != nil || claimed {
		t.Fatalf("probe admitted inside interval: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now.Add(GrokPaidQuotaProbeInterval)); err != nil || !claimed {
		t.Fatalf("probe not admitted after interval: claimed=%v err=%v", claimed, err)
	}
}

func TestClaimGrokPaidQuotaProbeWaitsForPeriodEnd(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	acc := &Account{AccountType: "grok", Enabled: true, GrokBilling: GrokBillingSnapshot{SyncedAt: now,
		Weekly: GrokQuotaWindow{HasUsage: true, UsagePercent: 100, ResetAt: now.Add(time.Hour)}}}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
	if err != nil || claimed {
		t.Fatalf("claim before period end=%v err=%v", claimed, err)
	}
}
