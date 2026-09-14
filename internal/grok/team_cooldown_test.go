package grok

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func withTestDistributedGrokLimits(t *testing.T) *redisGrokLimits {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	distributedGrokLimits.Lock()
	previous := distributedGrokLimits.backend
	distributedGrokLimits.backend = &redisGrokLimits{
		client: client,
		prefix: "test:grok:limits:",
		note: redis.NewScript(`
			local current = redis.call("PTTL", KEYS[1])
			local wanted = tonumber(ARGV[1])
			if current < wanted then redis.call("PSETEX", KEYS[1], wanted, "1") end
			return 1
		`),
	}
	backend := distributedGrokLimits.backend
	distributedGrokLimits.Unlock()
	t.Cleanup(func() {
		distributedGrokLimits.Lock()
		distributedGrokLimits.backend = previous
		distributedGrokLimits.Unlock()
		_ = client.Close()
		mini.Close()
	})
	return backend
}

func TestDistributedCooldownVisibleToAnotherRegistry(t *testing.T) {
	withTestDistributedGrokLimits(t)
	first := newTeamCooldownRegistry()
	second := newTeamCooldownRegistry()
	first.Note(RateLimitScopeRPM, "shared-team", "grok-4.6", time.Minute)
	if remaining := second.RetryAfterFor(RateLimitScopeRPM, "shared-team", "grok-4.6"); remaining <= 0 {
		t.Fatal("second process did not observe Redis cooldown")
	}
}

func TestDistributedPacingSerializesReplicas(t *testing.T) {
	backend := withTestDistributedGrokLimits(t)
	if err := backend.waitPacing(context.Background(), "shared-account", 2); err != nil {
		t.Fatalf("first pacing token failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := backend.waitPacing(ctx, "shared-account", 2); err == nil {
		t.Fatal("second replica bypassed distributed pacing interval")
	}
}

func TestTeamCooldownNoteAndRetry(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 30*time.Second)

	if remaining := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5"); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("remaining = %s, want in (0, 30s]", remaining)
	}
	// Different team/model must be unaffected.
	if remaining := registry.RetryAfterFor(RateLimitScopeRPS, "team-b", "grok-4.5"); remaining != 0 {
		t.Fatalf("team-b should have no cooldown, got %s", remaining)
	}
	if remaining := registry.RetryAfterFor(RateLimitScopeRPM, "team-a", "grok-4.5"); remaining != 0 {
		t.Fatalf("different scope should have no cooldown, got %s", remaining)
	}
}

func TestTeamCooldownNoteIgnoresEmptyTeamOrModel(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "", "grok-4.5", time.Minute)
	registry.Note(RateLimitScopeRPS, "team-a", "", time.Minute)
	if registry.RetryAfterFor(RateLimitScopeRPS, "", "grok-4.5") != 0 {
		t.Fatal("empty team must not create an entry")
	}
	if registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "") != 0 {
		t.Fatal("empty model must not create an entry")
	}
}

func TestTeamCooldownExpiry(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPM, "team-a", "grok-4.5", time.Nanosecond)
	time.Sleep(time.Millisecond)
	if remaining := registry.RetryAfterFor(RateLimitScopeRPM, "team-a", "grok-4.5"); remaining != 0 {
		t.Fatalf("expired cooldown should be cleared, got %s", remaining)
	}
}

func TestTeamCooldownNoteKeepsLongest(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 10*time.Second)
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 5*time.Second) // shorter: keep existing
	first := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5")
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 60*time.Second) // longer: extend
	second := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5")
	if second <= first {
		t.Fatalf("longer note should extend cooldown: first=%s second=%s", first, second)
	}
}

func TestTeamCooldownWaitBlocking(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPM, "team-a", "grok-4.5", 50*time.Millisecond)

	start := time.Now()
	err := registry.Wait(context.Background(), RateLimitScopeRPM, "team-a", "grok-4.5")
	if err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("wait returned too early: %s", elapsed)
	}
}

func TestTeamCooldownWaitCancel(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPM, "team-a", "grok-4.5", time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := registry.Wait(ctx, RateLimitScopeRPM, "team-a", "grok-4.5")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("wait did not honour cancellation promptly")
	}
}

func TestTeamCooldownWaitNoEntry(t *testing.T) {
	registry := newTeamCooldownRegistry()
	if err := registry.Wait(context.Background(), RateLimitScopeRPS, "team-x", "grok-4.5"); err != nil {
		t.Fatalf("wait without entry should return nil, got %v", err)
	}
}

func TestTeamCooldownConcurrentNote(t *testing.T) {
	registry := newTeamCooldownRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry.Note(RateLimitScopeRPS, "team-conc", "grok-4.5", time.Second)
		}()
	}
	wg.Wait()
	if remaining := registry.RetryAfterFor(RateLimitScopeRPS, "team-conc", "grok-4.5"); remaining <= 0 {
		t.Fatalf("concurrent notes should leave an entry, got %s", remaining)
	}
	registry.mu.Lock()
	size := len(registry.entries)
	registry.mu.Unlock()
	if size > teamCooldownMaxSize {
		t.Fatal("registry exceeded max size")
	}
}
