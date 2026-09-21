package tokencache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupRedisCache(t *testing.T, ttl time.Duration) (*RedisCache, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	cache := NewRedisCache(client, "test:", ttl)
	return cache, s
}

func TestRedisCacheGetPut(t *testing.T) {
	cache, _ := setupRedisCache(t, 5*time.Minute)
	ctx := context.Background()

	// Miss
	_, ok := cache.Get(ctx, "key1")
	if ok {
		t.Fatal("expected cache miss")
	}

	// Put and hit
	cache.Put(ctx, "key1", 42)
	tokens, ok := cache.Get(ctx, "key1")
	if !ok || tokens != 42 {
		t.Fatalf("expected 42, got %d (ok=%v)", tokens, ok)
	}

	// Overwrite
	cache.Put(ctx, "key1", 100)
	tokens, ok = cache.Get(ctx, "key1")
	if !ok || tokens != 100 {
		t.Fatalf("expected 100, got %d (ok=%v)", tokens, ok)
	}
}

func TestRedisCacheTTLExpiry(t *testing.T) {
	cache, mr := setupRedisCache(t, 1*time.Second)
	ctx := context.Background()

	cache.Put(ctx, "expire_me", 99)
	tokens, ok := cache.Get(ctx, "expire_me")
	if !ok || tokens != 99 {
		t.Fatal("expected hit before expiry")
	}

	// Fast-forward time in miniredis
	mr.FastForward(2 * time.Second)

	_, ok = cache.Get(ctx, "expire_me")
	if ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
}

func TestRedisCacheClear(t *testing.T) {
	cache, _ := setupRedisCache(t, 5*time.Minute)
	ctx := context.Background()

	cache.Put(ctx, "a", 1)
	cache.Put(ctx, "b", 2)
	cache.Put(ctx, "c", 3)

	count, _, err := cache.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 entries, got %d", count)
	}

	if err := cache.Clear(ctx); err != nil {
		t.Fatal(err)
	}

	count, _, err = cache.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 entries after clear, got %d", count)
	}
}

func TestRedisCacheClearPreservesOtherPrefixes(t *testing.T) {
	cache, mr := setupRedisCache(t, 5*time.Minute)
	ctx := context.Background()

	cache.Put(ctx, "owned", 1)
	mr.Set("other:key", "keep")

	if err := cache.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if mr.Exists("test:tcache:owned") {
		t.Fatal("expected cache-owned key to be unlinked")
	}
	if !mr.Exists("other:key") {
		t.Fatal("clear removed key outside cache prefix")
	}
}

func TestRedisCacheSetTTL(t *testing.T) {
	cache, mr := setupRedisCache(t, 10*time.Second)
	ctx := context.Background()

	cache.Put(ctx, "old_ttl", 1)

	// Change TTL to 1 second
	cache.SetTTL(1 * time.Second)
	cache.Put(ctx, "new_ttl", 2)

	mr.FastForward(2 * time.Second)

	// Old key should still exist (10s TTL)
	_, ok := cache.Get(ctx, "old_ttl")
	if !ok {
		t.Fatal("old key should still exist")
	}

	// New key should be expired (1s TTL)
	_, ok = cache.Get(ctx, "new_ttl")
	if ok {
		t.Fatal("new key should have expired")
	}
}

func TestRedisCacheGetStats(t *testing.T) {
	cache, _ := setupRedisCache(t, 5*time.Minute)
	ctx := context.Background()

	count, _, err := cache.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	cache.Put(ctx, "x", 10)
	cache.Put(ctx, "y", 20)

	count, size, err := cache.GetStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 entries, got %d", count)
	}
	if size <= 0 {
		t.Fatal("expected positive size")
	}
}

func TestRedisCacheNilSafe(t *testing.T) {
	var cache *RedisCache
	ctx := context.Background()

	// All operations should be no-ops on nil
	_, ok := cache.Get(ctx, "key")
	if ok {
		t.Fatal("nil cache should return miss")
	}
	cache.Put(ctx, "key", 1)
	cache.SetTTL(time.Minute)
	count, _, _ := cache.GetStats(ctx)
	if count != 0 {
		t.Fatal("nil cache stats should be zero")
	}
	if err := cache.Clear(ctx); err != nil {
		t.Fatal("nil cache clear should not error")
	}
}
