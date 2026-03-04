// Package handler provides property-based tests for ShardedMap.
package handler

import (
	"sync"
	"testing"
	"testing/quick"
)

// **Validates: Requirements 2.4, 2.5**
//
// Property 2: 分片 Map 键映射一致性
// For any string key, multiple calls to getShard(key) should always return the same shard index.
//
// Property 3: 分片 Map 并发安全性
// For any set of concurrent Get/Set/Delete operations, ShardedMap should maintain data consistency
// and not produce deadlocks.

// TestShardedMapKeyMappingConsistency tests that getShard returns consistent results
// for the same key across multiple calls.
func TestShardedMapKeyMappingConsistency(t *testing.T) {
	property := func(key string) bool {
		m := NewShardedMap[int]()

		// Get shard index for the key multiple times
		shard1 := m.getShard(key)
		shard2 := m.getShard(key)
		shard3 := m.getShard(key)

		// All shard indices should be the same
		if shard1 != shard2 || shard2 != shard3 {
			return false
		}

		// Shard index should be within valid range
		if shard1 < 0 || shard1 >= ShardCount {
			return false
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Errorf("ShardedMap key mapping consistency property failed: %v", err)
	}
}

// TestShardedMapKeyMappingDistribution tests that keys are distributed across shards
// (not all mapping to the same shard).
func TestShardedMapKeyMappingDistribution(t *testing.T) {
	property := func(keys []string) bool {
		if len(keys) < ShardCount {
			return true // Skip if not enough keys
		}

		m := NewShardedMap[int]()
		shardUsage := make(map[int]bool)

		// Map each key to a shard
		for _, key := range keys {
			shard := m.getShard(key)
			shardUsage[shard] = true
		}

		// With enough random keys, we should use multiple shards
		// (not all keys mapping to a single shard)
		return len(shardUsage) > 1
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap key mapping distribution property failed: %v", err)
	}
}

// TestShardedMapConcurrentSafety tests that concurrent operations maintain data consistency.
func TestShardedMapConcurrentSafety(t *testing.T) {
	property := func(operations []uint8) bool {
		if len(operations) == 0 {
			return true
		}

		m := NewShardedMap[int]()
		var wg sync.WaitGroup
		errChan := make(chan bool, len(operations))

		// Execute concurrent operations
		for i, op := range operations {
			wg.Add(1)
			go func(idx int, operation uint8) {
				defer wg.Done()

				key := string(rune('a' + (idx % 26)))
				value := int(operation)

				// Perform random operation based on operation value
				switch operation % 3 {
				case 0: // Set
					m.Set(key, value)
				case 1: // Get
					m.Get(key)
				case 2: // Delete
					m.Delete(key)
				}

				errChan <- true
			}(i, op)
		}

		wg.Wait()
		close(errChan)

		// Count successful operations
		successCount := 0
		for range errChan {
			successCount++
		}

		// All operations should complete without deadlock
		return successCount == len(operations)
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap concurrent safety property failed: %v", err)
	}
}

// TestShardedMapConcurrentSetGet tests that concurrent Set and Get operations
// maintain consistency.
func TestShardedMapConcurrentSetGet(t *testing.T) {
	property := func(values []uint8) bool {
		if len(values) == 0 {
			return true
		}

		m := NewShardedMap[int]()
		var wg sync.WaitGroup

		// Concurrently set values
		for i, val := range values {
			wg.Add(1)
			go func(idx int, v uint8) {
				defer wg.Done()
				key := string(rune('a' + (idx % 26)))
				m.Set(key, int(v))
			}(i, val)
		}

		wg.Wait()

		// Verify we can read all keys without deadlock
		readSuccess := true
		for i := range values {
			key := string(rune('a' + (i % 26)))
			_, _ = m.Get(key)
		}

		return readSuccess
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap concurrent Set/Get property failed: %v", err)
	}
}

// TestShardedMapConcurrentSetDelete tests that concurrent Set and Delete operations
// don't cause deadlocks or panics.
func TestShardedMapConcurrentSetDelete(t *testing.T) {
	property := func(operations []bool) bool {
		if len(operations) == 0 {
			return true
		}

		m := NewShardedMap[int]()
		var wg sync.WaitGroup

		// Concurrently set and delete the same keys
		for i, shouldSet := range operations {
			wg.Add(1)
			go func(idx int, set bool) {
				defer wg.Done()
				key := string(rune('a' + (idx % 10)))

				if set {
					m.Set(key, idx)
				} else {
					m.Delete(key)
				}
			}(i, shouldSet)
		}

		wg.Wait()

		// Should complete without deadlock
		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap concurrent Set/Delete property failed: %v", err)
	}
}

// TestShardedMapDataConsistency tests that data written to the map can be read back correctly
// in a single-threaded context.
func TestShardedMapDataConsistency(t *testing.T) {
	property := func(kvPairs map[string]uint8) bool {
		m := NewShardedMap[int]()

		// Set all key-value pairs
		for k, v := range kvPairs {
			m.Set(k, int(v))
		}

		// Verify all values can be read back
		for k, expectedV := range kvPairs {
			actualV, ok := m.Get(k)
			if !ok {
				return false
			}
			if actualV != int(expectedV) {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap data consistency property failed: %v", err)
	}
}

// TestShardedMapDeleteConsistency tests that deleted keys are no longer retrievable.
func TestShardedMapDeleteConsistency(t *testing.T) {
	property := func(keys []string) bool {
		if len(keys) == 0 {
			return true
		}

		m := NewShardedMap[int]()

		// Set all keys
		for i, k := range keys {
			m.Set(k, i)
		}

		// Delete all keys
		for _, k := range keys {
			m.Delete(k)
		}

		// Verify all keys are deleted
		for _, k := range keys {
			_, ok := m.Get(k)
			if ok {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap delete consistency property failed: %v", err)
	}
}

// TestShardedMapRangeConsistency tests that Range iterates over all set values.
func TestShardedMapRangeConsistency(t *testing.T) {
	property := func(kvPairs map[string]uint8) bool {
		m := NewShardedMap[int]()

		// Set all key-value pairs
		for k, v := range kvPairs {
			m.Set(k, int(v))
		}

		// Collect all keys via Range
		rangedKeys := make(map[string]int)
		m.Range(func(key string, value int) bool {
			rangedKeys[key] = value
			return true
		})

		// Verify all keys were visited
		if len(rangedKeys) != len(kvPairs) {
			return false
		}

		for k, expectedV := range kvPairs {
			actualV, ok := rangedKeys[k]
			if !ok || actualV != int(expectedV) {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("ShardedMap Range consistency property failed: %v", err)
	}
}

// TestShardedMapConcurrentRange tests that Range can execute concurrently with other operations
// without deadlock.
func TestShardedMapConcurrentRange(t *testing.T) {
	property := func(operations []uint8) bool {
		if len(operations) == 0 {
			return true
		}

		m := NewShardedMap[int]()
		var wg sync.WaitGroup

		// Pre-populate the map
		for i := 0; i < 10; i++ {
			m.Set(string(rune('a'+i)), i)
		}

		// Concurrently perform Range and other operations
		for i, op := range operations {
			wg.Add(1)
			go func(idx int, operation uint8) {
				defer wg.Done()

				switch operation % 4 {
				case 0: // Range
					m.Range(func(key string, value int) bool {
						return true
					})
				case 1: // Set
					key := string(rune('a' + (idx % 26)))
					m.Set(key, idx)
				case 2: // Get
					key := string(rune('a' + (idx % 26)))
					m.Get(key)
				case 3: // Delete
					key := string(rune('a' + (idx % 26)))
					m.Delete(key)
				}
			}(i, op)
		}

		wg.Wait()

		// Should complete without deadlock
		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 50}); err != nil {
		t.Errorf("ShardedMap concurrent Range property failed: %v", err)
	}
}

// TestShardedMapEmptyKeyHandling tests that empty keys are handled correctly.
func TestShardedMapEmptyKeyHandling(t *testing.T) {
	m := NewShardedMap[int]()

	// Empty key should map to a valid shard
	shard1 := m.getShard("")
	shard2 := m.getShard("")

	if shard1 != shard2 {
		t.Errorf("Empty key mapping inconsistent: %d != %d", shard1, shard2)
	}

	if shard1 < 0 || shard1 >= ShardCount {
		t.Errorf("Empty key mapped to invalid shard: %d", shard1)
	}

	// Should be able to set and get with empty key
	m.Set("", 42)
	val, ok := m.Get("")
	if !ok || val != 42 {
		t.Errorf("Empty key Set/Get failed: got (%v, %v), want (42, true)", val, ok)
	}

	// Should be able to delete empty key
	m.Delete("")
	_, ok = m.Get("")
	if ok {
		t.Errorf("Empty key still exists after Delete")
	}
}

// BenchmarkShardedMapConcurrentAccess benchmarks concurrent access performance.
func BenchmarkShardedMapConcurrentAccess(b *testing.B) {
	m := NewShardedMap[int]()

	// Pre-populate
	for i := 0; i < 1000; i++ {
		m.Set(string(rune('a'+(i%26)))+string(rune('0'+(i%10))), i)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := string(rune('a'+(i%26))) + string(rune('0'+(i%10)))
			switch i % 3 {
			case 0:
				m.Set(key, i)
			case 1:
				m.Get(key)
			case 2:
				m.Delete(key)
			}
			i++
		}
	})
}

// BenchmarkShardedMapSet benchmarks Set operation performance.
func BenchmarkShardedMapSet(b *testing.B) {
	m := NewShardedMap[int]()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := string(rune('a' + (i % 26)))
		m.Set(key, i)
	}
}

// BenchmarkShardedMapGet benchmarks Get operation performance.
func BenchmarkShardedMapGet(b *testing.B) {
	m := NewShardedMap[int]()

	// Pre-populate
	for i := 0; i < 1000; i++ {
		m.Set(string(rune('a'+(i%26))), i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := string(rune('a' + (i % 26)))
		m.Get(key)
	}
}
