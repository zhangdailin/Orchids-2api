// Package tokencache provides property-based tests for LRU cache.
package tokencache

import (
	"context"
	"fmt"
	"testing"
	"testing/quick"
	"time"
)

// **Validates: Requirements 6.1, 6.2**
//
// Property 4: LRU 驱逐正确性
// For any cache state, when the cache is full and a new entry is inserted,
// the evicted entry should be the one with the earliest access time.

// TestLRUEvictionCorrectness tests that the cache always evicts the least recently accessed item
// when the cache is full.
func TestLRUEvictionCorrectness(t *testing.T) {
	property := func(seed uint8) bool {
		ctx := context.Background()

		// Create a cache with max 3 entries and no TTL
		cache := NewMemoryCache(0, 3)
		defer cache.Clear(ctx)

		// Use seed to generate deterministic but varied test cases
		numOps := int(seed%5) + 4 // 4-8 operations

		// Track last access time for each key
		keyLastAccessTime := make(map[string]int)
		currentKeys := make(map[string]bool)

		for i := 0; i < numOps; i++ {
			key := fmt.Sprintf("key%d", (seed+uint8(i))%5)
			value := int(seed)*100 + i

			// Alternate between Put and Get operations
			if i%2 == 0 || !currentKeys[key] {
				// Put operation
				cache.Put(ctx, key, value)

				// Update last access time
				keyLastAccessTime[key] = i
				currentKeys[key] = true

				// If we exceed capacity, find which key should have been evicted
				if len(currentKeys) > 3 {
					// Find the key with the earliest access time
					var lruKey string
					minTime := numOps + 1
					for k := range currentKeys {
						if keyLastAccessTime[k] < minTime {
							minTime = keyLastAccessTime[k]
							lruKey = k
						}
					}

					// Remove from tracking
					delete(currentKeys, lruKey)
					delete(keyLastAccessTime, lruKey)

					// Verify the LRU key was actually evicted from cache
					if _, ok := cache.Get(ctx, lruKey); ok {
						return false // LRU key should have been evicted
					}
				}
			} else {
				// Get operation - updates access time
				if val, ok := cache.Get(ctx, key); ok {
					// Verify the value is correct
					if val <= 0 {
						return false
					}
					// Update last access time
					keyLastAccessTime[key] = i
				}
			}

			// Small delay to ensure different access times
			time.Sleep(time.Millisecond)
		}

		// Verify cache size doesn't exceed max
		count, _, _ := cache.GetStats(ctx)
		return count <= 3
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("LRU eviction correctness property failed: %v", err)
	}
}

// TestLRUAccessTimeUpdate tests that accessing a cache entry updates its access timestamp
func TestLRUAccessTimeUpdate(t *testing.T) {
	property := func(seed uint8) bool {
		ctx := context.Background()

		// Create a cache with max 2 entries
		cache := NewMemoryCache(0, 2)
		defer cache.Clear(ctx)

		// Add two items
		cache.Put(ctx, "first", 100)
		time.Sleep(5 * time.Millisecond)
		cache.Put(ctx, "second", 200)
		time.Sleep(5 * time.Millisecond)

		// Access the first item multiple times to ensure accessedAt is updated (sampled LRU)
		for i := 0; i < 8; i++ {
			if _, ok := cache.Get(ctx, "first"); !ok {
				return false
			}
		}
		time.Sleep(5 * time.Millisecond)

		// Add a third item - should evict "second" (not "first" since we just accessed it)
		cache.Put(ctx, "third", 300)

		// Verify "first" still exists (was accessed recently)
		if _, ok := cache.Get(ctx, "first"); !ok {
			return false
		}

		// Verify "second" was evicted (least recently accessed)
		if _, ok := cache.Get(ctx, "second"); ok {
			return false // "second" should have been evicted
		}

		// Verify "third" exists
		if _, ok := cache.Get(ctx, "third"); !ok {
			return false
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("LRU access time update property failed: %v", err)
	}
}

// TestLRUEvictionWithRandomAccess tests LRU eviction with random access patterns
func TestLRUEvictionWithRandomAccess(t *testing.T) {
	property := func(accessPattern []uint8) bool {
		if len(accessPattern) < 5 {
			return true // Skip too short patterns
		}

		ctx := context.Background()

		// Create a cache with max 3 entries
		cache := NewMemoryCache(0, 3)
		defer cache.Clear(ctx)

		// Track which keys exist and their last access index
		keyLastAccess := make(map[string]int)
		keyExists := make(map[string]bool)

		for i, pattern := range accessPattern {
			key := fmt.Sprintf("k%d", pattern%5) // 5 possible keys
			value := i * 10

			// Perform operation based on pattern
			if pattern%3 == 0 && keyExists[key] {
				// Get operation - do 8 Gets to ensure sampled LRU updates accessedAt
				if _, ok := cache.Get(ctx, key); ok {
					keyLastAccess[key] = i
					for j := 0; j < 7; j++ {
						cache.Get(ctx, key)
					}
				}
			} else {
				// Put operation
				cache.Put(ctx, key, value)
				keyLastAccess[key] = i
				keyExists[key] = true

				// If we have more than 3 keys tracked, find and remove LRU
				if len(keyExists) > 3 {
					var lruKey string
					lruIndex := len(accessPattern)

					for k := range keyExists {
						if keyLastAccess[k] < lruIndex {
							lruIndex = keyLastAccess[k]
							lruKey = k
						}
					}

					delete(keyExists, lruKey)
					delete(keyLastAccess, lruKey)
				}
			}

			time.Sleep(time.Millisecond)
		}

		// Verify cache size
		count, _, _ := cache.GetStats(ctx)
		if count > 3 {
			return false
		}

		// Verify all tracked keys exist in cache
		for key := range keyExists {
			if _, ok := cache.Get(ctx, key); !ok {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 50}); err != nil {
		t.Errorf("LRU eviction with random access property failed: %v", err)
	}
}

// TestLRUEvictionSequential tests that sequential puts without gets evict in FIFO order
func TestLRUEvictionSequential(t *testing.T) {
	property := func(count uint8) bool {
		n := int(count%10) + 4 // 4-13 items

		ctx := context.Background()

		// Create a cache with max 3 entries
		cache := NewMemoryCache(0, 3)
		defer cache.Clear(ctx)

		// Add items sequentially
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("item%d", i)
			cache.Put(ctx, key, i*100)
			time.Sleep(time.Millisecond)
		}

		// Only the last 3 items should exist
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("item%d", i)
			_, exists := cache.Get(ctx, key)

			if i < n-3 {
				// Older items should be evicted
				if exists {
					return false
				}
			} else {
				// Last 3 items should exist
				if !exists {
					return false
				}
			}
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("LRU sequential eviction property failed: %v", err)
	}
}

// TestLRUEvictionWithUpdates tests that updating an existing key updates its access time
func TestLRUEvictionWithUpdates(t *testing.T) {
	property := func(seed uint8) bool {
		ctx := context.Background()

		// Create a cache with max 2 entries
		cache := NewMemoryCache(0, 2)
		defer cache.Clear(ctx)

		// Add two items
		cache.Put(ctx, "a", 1)
		time.Sleep(5 * time.Millisecond)
		cache.Put(ctx, "b", 2)
		time.Sleep(5 * time.Millisecond)

		// Update "a" (this should update its access time)
		cache.Put(ctx, "a", 10)
		time.Sleep(5 * time.Millisecond)

		// Add a third item - should evict "b" (not "a" since we just updated it)
		cache.Put(ctx, "c", 3)

		// Verify "a" still exists with updated value
		if val, ok := cache.Get(ctx, "a"); !ok || val != 10 {
			return false
		}

		// Verify "b" was evicted
		if _, ok := cache.Get(ctx, "b"); ok {
			return false
		}

		// Verify "c" exists
		if val, ok := cache.Get(ctx, "c"); !ok || val != 3 {
			return false
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("LRU eviction with updates property failed: %v", err)
	}
}

// TestLRUCacheSizeInvariant tests that cache never exceeds max size
func TestLRUCacheSizeInvariant(t *testing.T) {
	property := func(operations []uint8) bool {
		if len(operations) == 0 {
			return true
		}

		ctx := context.Background()
		maxSize := 5

		cache := NewMemoryCache(0, maxSize)
		defer cache.Clear(ctx)

		for i, op := range operations {
			key := fmt.Sprintf("k%d", op%10)
			value := i * 100

			if op%2 == 0 {
				cache.Put(ctx, key, value)
			} else {
				cache.Get(ctx, key)
			}

			// Verify size invariant
			count, _, _ := cache.GetStats(ctx)
			if count > int64(maxSize) {
				return false
			}

			time.Sleep(time.Millisecond)
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 50}); err != nil {
		t.Errorf("LRU cache size invariant property failed: %v", err)
	}
}
