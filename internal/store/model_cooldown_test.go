package store

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// TestModelCooldown_ScopedToModelNotAccount is the rule the pool depends on: a
// throttled model must be skipped for that model while the account stays usable
// for every other model.
func TestModelCooldown_ScopedToModelNotAccount(t *testing.T) {
	now := time.Now()
	acc := &Account{
		AccountType: "grok",
		ModelCooldowns: map[string]time.Time{
			"grok-4.6":      now.Add(90 * time.Second),
			"grok-heavy":    now.Add(time.Hour),
			"grok-expired":  now.Add(-time.Minute),
		},
	}

	if got := ModelCooldownRemaining(acc, "grok-4.6", now); got <= 0 {
		t.Fatalf("throttled model remaining = %v, want > 0", got)
	}
	if got := ModelCooldownRemaining(acc, "grok-4.5", now); got != 0 {
		t.Fatalf("untouched model remaining = %v, want 0", got)
	}
	if got := ModelCooldownRemaining(acc, "grok-expired", now); got != 0 {
		t.Fatalf("expired cooldown remaining = %v, want 0", got)
	}
	if got := ModelCooldownRemaining(nil, "grok-4.6", now); got != 0 {
		t.Fatalf("nil account remaining = %v, want 0", got)
	}
}

// TestRecordModelCooldown_SetsOnlyThatModel keeps the write path honest: a
// throttle for one model must never mark the account or another model.
func TestRecordModelCooldown_SetsOnlyThatModel(t *testing.T) {
	now := time.Now()
	acc := &Account{AccountType: "grok"}

	RecordModelCooldown(acc, "grok-4.6", now.Add(time.Minute))
	if remaining := ModelCooldownRemaining(acc, "grok-4.6", now); remaining <= 0 {
		t.Fatal("the throttled model must be marked")
	}
	if remaining := ModelCooldownRemaining(acc, "grok-4.5", now); remaining != 0 {
		t.Fatal("an unrelated model must stay available")
	}
	if acc.StatusCode != "" {
		t.Fatalf("a model-scoped throttle must not set the account status, got %q", acc.StatusCode)
	}

	// A later deadline wins; an earlier one does not shorten it.
	RecordModelCooldown(acc, "grok-4.6", now.Add(5*time.Minute))
	if got := ModelCooldownRemaining(acc, "grok-4.6", now); got < 4*time.Minute {
		t.Fatalf("remaining = %v, want the later deadline", got)
	}
	RecordModelCooldown(acc, "grok-4.6", now.Add(10*time.Second))
	if got := ModelCooldownRemaining(acc, "grok-4.6", now); got < 4*time.Minute {
		t.Fatalf("an earlier deadline shortened the cooldown to %v", got)
	}

	// A past deadline is not recorded at all.
	before := len(acc.ModelCooldowns)
	RecordModelCooldown(acc, "grok-4.7", now.Add(-time.Minute))
	if len(acc.ModelCooldowns) != before {
		t.Fatal("an expired deadline must not be recorded")
	}
	RecordModelCooldown(acc, "  ", now.Add(time.Minute))
	if len(acc.ModelCooldowns) != before {
		t.Fatal("an empty model name must not be recorded")
	}
}

// TestMergeModelCooldowns_KeepsLatestAndDropsExpired covers the write path: a
// partial update must not erase a cooldown another path just recorded, and dead
// entries must not accumulate.
func TestMergeModelCooldowns_KeepsLatestAndDropsExpired(t *testing.T) {
	now := time.Now()
	merged := mergeModelCooldowns(
		map[string]time.Time{
			"grok-4.6":   now.Add(30 * time.Second),
			"grok-stale": now.Add(-time.Hour),
		},
		map[string]time.Time{
			"grok-4.6":  now.Add(5 * time.Minute),
			"grok-4.5":  now.Add(time.Minute),
			"":          now.Add(time.Hour),
		},
	)
	if len(merged) != 2 {
		t.Fatalf("merged = %v, want two live models", merged)
	}
	if merged["grok-4.6"].Sub(now) < 4*time.Minute {
		t.Fatalf("grok-4.6 = %v, want the later deadline", merged["grok-4.6"].Sub(now))
	}
	if _, dead := merged["grok-stale"]; dead {
		t.Fatal("an expired cooldown must not be carried forward")
	}
	if mergeModelCooldowns(nil, nil) != nil {
		t.Fatal("empty input must not create an empty map")
	}
}

// TestUpdateAccount_PreservesModelCooldownsAcrossPartialWrites pins the guard:
// saving an unrelated field (enabled, weight) keeps cooldowns in Redis.
func TestUpdateAccount_PreservesModelCooldownsAcrossPartialWrites(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "modelcool:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := t.Context()
	acc := &Account{
		AccountType: "grok",
		Enabled:     true,
		ModelCooldowns: map[string]time.Time{
			"grok-4.6": time.Now().Add(2 * time.Minute),
		},
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	// A request path that knows nothing about model cooldowns saves the account.
	stale, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	stale.Weight = 3
	stale.ModelCooldowns = nil
	if err := s.UpdateAccount(ctx, stale); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	after, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if after.Weight != 3 {
		t.Fatalf("weight = %d, want the partial update applied", after.Weight)
	}
	if remaining := ModelCooldownRemaining(after, "grok-4.6", time.Now()); remaining <= 0 {
		t.Fatal("a partial write erased the model cooldown")
	}
}
