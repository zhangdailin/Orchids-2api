package handler

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupRedisSessionStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 30*time.Minute)
	return store, s
}

func TestRedisSessionStoreConvID(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	// Miss
	_, ok := store.GetConvID(ctx, "session1")
	if ok {
		t.Fatal("expected miss")
	}

	// Set and get
	store.SetConvID(ctx, "session1", "conv_abc123")
	id, ok := store.GetConvID(ctx, "session1")
	if !ok || id != "conv_abc123" {
		t.Fatalf("expected conv_abc123, got %q (ok=%v)", id, ok)
	}
}

func TestRedisSessionStoreDelete(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	store.SetConvID(ctx, "session1", "conv_xyz")

	store.DeleteSession(ctx, "session1")

	_, ok := store.GetConvID(ctx, "session1")
	if ok {
		t.Fatal("convID should be deleted")
	}
}

func TestRedisSessionStoreTTL(t *testing.T) {
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 1*time.Second)
	ctx := context.Background()

	store.SetConvID(ctx, "session1", "conv_ttl")
	s.FastForward(2 * time.Second)

	_, ok := store.GetConvID(ctx, "session1")
	if ok {
		t.Fatal("session should have expired")
	}
}

func TestRedisSessionStoreTouch(t *testing.T) {
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 3*time.Second)
	ctx := context.Background()

	store.SetConvID(ctx, "session1", "conv_touch")

	// Advance 2 seconds, then touch
	s.FastForward(2 * time.Second)
	store.Touch(ctx, "session1")

	// Advance another 2 seconds (total 4s from creation, but only 2s from touch)
	s.FastForward(2 * time.Second)

	_, ok := store.GetConvID(ctx, "session1")
	if !ok {
		t.Fatal("session should still exist after touch")
	}
}

// --- Memory tests ---
func TestMemorySessionStoreCleanup(t *testing.T) {
	store := NewMemorySessionStore(100*time.Millisecond, 100)
	ctx := context.Background()

	store.SetConvID(ctx, "s1", "conv_cleanup")
	time.Sleep(150 * time.Millisecond)
	store.Cleanup(ctx)

	if _, ok := store.GetConvID(ctx, "s1"); ok {
		t.Fatal("session should have been cleaned up")
	}
}

func TestMemorySessionStoreAccountID(t *testing.T) {
	store := NewMemorySessionStore(30*time.Minute, 100)
	ctx := context.Background()

	if _, ok := store.GetAccountID(ctx, "session"); ok {
		t.Fatal("expected account id miss")
	}
	store.SetAccountID(ctx, "session", 132)

	if accountID, ok := store.GetAccountID(ctx, "session"); !ok || accountID != 132 {
		t.Fatalf("accountID=%d ok=%v, want 132 true", accountID, ok)
	}
}

func TestRedisSessionStoreAccountID(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	if _, ok := store.GetAccountID(ctx, "session"); ok {
		t.Fatal("expected account id miss")
	}
	store.SetAccountID(ctx, "session", 132)

	if accountID, ok := store.GetAccountID(ctx, "session"); !ok || accountID != 132 {
		t.Fatalf("accountID=%d ok=%v, want 132 true", accountID, ok)
	}
}
