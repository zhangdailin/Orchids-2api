package tokencache

import (
	"context"
	"testing"
	"time"
)

// TestLRUEviction verifies that the cache evicts the least recently accessed item
func TestMemoryCacheCloseStopsCleanup(t *testing.T) {
	cache := NewMemoryCache(time.Minute)
	cache.Close()

	select {
	case <-cache.stopped:
	default:
		t.Fatal("Close returned before cleanup goroutine stopped")
	}

	// Close is idempotent and must not panic or block.
	cache.Close()
}

func TestMemoryPromptCacheCloseStopsCleanup(t *testing.T) {
	cache := NewMemoryPromptCache(time.Minute)
	cache.Close()

	select {
	case <-cache.stopped:
	default:
		t.Fatal("Close returned before cleanup goroutine stopped")
	}

	cache.Close()
}

func TestLRUEviction(t *testing.T) {
	ctx := context.Background()

	// Create a cache with max 3 entries and no TTL
	cache := NewMemoryCache(0, 3)
	defer cache.Clear(ctx)

	// Add 3 items
	cache.Put(ctx, "key1", 100)
	time.Sleep(10 * time.Millisecond)
	cache.Put(ctx, "key2", 200)
	time.Sleep(10 * time.Millisecond)
	cache.Put(ctx, "key3", 300)

	// Access key1 multiple times to ensure accessedAt is updated (sampled LRU updates 1-in-8)
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 8; i++ {
		if val, ok := cache.Get(ctx, "key1"); !ok || val != 100 {
			t.Fatalf("Expected key1=100, got %v, %v", val, ok)
		}
	}

	// Access key2 multiple times to ensure accessedAt is updated
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 8; i++ {
		if val, ok := cache.Get(ctx, "key2"); !ok || val != 200 {
			t.Fatalf("Expected key2=200, got %v, %v", val, ok)
		}
	}

	// Now key3 is the least recently accessed (only Put, no Get)
	// Add a 4th item, which should evict key3
	time.Sleep(10 * time.Millisecond)
	cache.Put(ctx, "key4", 400)

	// Verify key3 was evicted
	if _, ok := cache.Get(ctx, "key3"); ok {
		t.Error("Expected key3 to be evicted, but it still exists")
	}

	// Verify key1, key2, and key4 still exist
	if val, ok := cache.Get(ctx, "key1"); !ok || val != 100 {
		t.Errorf("Expected key1=100, got %v, %v", val, ok)
	}
	if val, ok := cache.Get(ctx, "key2"); !ok || val != 200 {
		t.Errorf("Expected key2=200, got %v, %v", val, ok)
	}
	if val, ok := cache.Get(ctx, "key4"); !ok || val != 400 {
		t.Errorf("Expected key4=400, got %v, %v", val, ok)
	}
}

// TestLRUEvictionWithoutAccess verifies eviction based on Put time when no Gets occur
func TestLRUEvictionWithoutAccess(t *testing.T) {
	ctx := context.Background()

	// Create a cache with max 2 entries
	cache := NewMemoryCache(0, 2)
	defer cache.Clear(ctx)

	// Add 2 items with delays to ensure different access times
	cache.Put(ctx, "oldest", 1)
	time.Sleep(10 * time.Millisecond)
	cache.Put(ctx, "newer", 2)

	// Add a 3rd item, should evict "oldest"
	time.Sleep(10 * time.Millisecond)
	cache.Put(ctx, "newest", 3)

	// Verify "oldest" was evicted
	if _, ok := cache.Get(ctx, "oldest"); ok {
		t.Error("Expected 'oldest' to be evicted")
	}

	// Verify "newer" and "newest" still exist
	if val, ok := cache.Get(ctx, "newer"); !ok || val != 2 {
		t.Errorf("Expected newer=2, got %v, %v", val, ok)
	}
	if val, ok := cache.Get(ctx, "newest"); !ok || val != 3 {
		t.Errorf("Expected newest=3, got %v, %v", val, ok)
	}
}
