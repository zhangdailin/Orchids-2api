package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// TestAccountFreeQuotaPersistsAndSurvivesPartialUpdates pins the two persistence
// rules the confirmed Free window depends on: it round-trips through Redis, and an
// unrelated partial update (a request counter, a credential rotation) cannot erase it.
func TestAccountFreeQuotaPersistsAndSurvivesPartialUpdates(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "free-quota:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	confirmedAt := time.Now().UTC().Truncate(time.Second)
	acc := &Account{
		Name:             "build-free",
		AccountType:      "grok",
		CredentialType:   "oauth",
		GrokProvider:     "build",
		OAuthAccessToken: "access",
		Enabled:          true,
		Weight:           1,
		GrokFreeQuota: GrokFreeQuotaSnapshot{
			Used: 500123, Limit: 500000, HasLimit: true,
			ConfirmedAt: confirmedAt,
			ResetAt:     confirmedAt.Add(24 * time.Hour),
		},
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	stored, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !stored.GrokFreeQuota.HasLimit || stored.GrokFreeQuota.Used != 500123 || stored.GrokFreeQuota.Limit != 500000 {
		t.Fatalf("stored free quota = %+v, want the confirmed 500123/500000 pair", stored.GrokFreeQuota)
	}
	if !stored.GrokFreeQuota.ConfirmedAt.Equal(confirmedAt) {
		t.Fatalf("confirmedAt = %v, want %v", stored.GrokFreeQuota.ConfirmedAt, confirmedAt)
	}

	// A partial update that never mentions the free window must keep it: the window is
	// confirmed once per exhaustion and must not be lost by the next request counter.
	partial := *stored
	partial.GrokFreeQuota = GrokFreeQuotaSnapshot{}
	partial.RequestCount = stored.RequestCount + 1
	if err := s.UpdateAccount(ctx, &partial); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	after, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !after.GrokFreeQuota.HasLimit || after.GrokFreeQuota.Limit != 500000 {
		t.Fatalf("a partial update erased the confirmed free window: %+v", after.GrokFreeQuota)
	}
}
