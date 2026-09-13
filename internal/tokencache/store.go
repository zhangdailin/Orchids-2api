package tokencache

import (
	"sync"
	"sync/atomic"
	"time"
)

// memoryItem is a single entry of a memoryStore. The payload T holds the
// cache-specific data and never participates in eviction or expiry.
type memoryItem[T any] struct {
	value      T
	expiresAt  time.Time
	accessedAt time.Time
	size       int64
}

// memoryStore is the shared LRU/TTL bookkeeping for the in-memory caches.
// T is the per-entry payload; it does not participate in eviction.
//
// Callers are responsible for holding s.mu when calling the *Locked helpers.
type memoryStore[T any] struct {
	mu          sync.RWMutex
	ttl         time.Duration
	maxEntries  int
	items       map[string]memoryItem[T]
	sizeBytes   int64
	accessCount atomic.Uint64
}

// newMemoryStore builds a store with an empty item map, normalising ttl and the
// optional entry cap exactly like the individual constructors used to.
func newMemoryStore[T any](ttl time.Duration, maxEntries ...int) memoryStore[T] {
	if ttl < 0 {
		ttl = 0
	}
	limit := 0
	if len(maxEntries) > 0 && maxEntries[0] > 0 {
		limit = maxEntries[0]
	}
	return memoryStore[T]{
		ttl:        ttl,
		maxEntries: limit,
		items:      make(map[string]memoryItem[T]),
	}
}

// expiredLocked reports whether item is past its TTL. Requires s.ttl > 0 to be
// meaningful; entries with a zero expiresAt never expire.
func (s *memoryStore[T]) expiredLocked(item memoryItem[T], now time.Time) bool {
	return s.ttl > 0 && !item.expiresAt.IsZero() && now.After(item.expiresAt)
}

// setTTL applies a new TTL, dropping every entry when the value changes.
func (s *memoryStore[T]) setTTL(ttl time.Duration) {
	if ttl < 0 {
		ttl = 0
	}
	s.mu.Lock()
	if s.ttl != ttl {
		s.ttl = ttl
		s.items = make(map[string]memoryItem[T])
		s.sizeBytes = 0
	}
	s.mu.Unlock()
}

// evictLRULocked drops the least recently accessed entry. Requires s.mu held.
func (s *memoryStore[T]) evictLRULocked() {
	var lruKey string
	var lruTime time.Time
	first := true
	for k, item := range s.items {
		if first || item.accessedAt.Before(lruTime) {
			lruKey = k
			lruTime = item.accessedAt
			first = false
		}
	}
	if !first {
		s.sizeBytes -= s.items[lruKey].size
		delete(s.items, lruKey)
	}
}

// pruneExpiredLocked removes every expired entry. Requires s.mu held.
func (s *memoryStore[T]) pruneExpiredLocked(now time.Time) {
	if s.ttl <= 0 {
		return
	}
	for key, item := range s.items {
		if !item.expiresAt.IsZero() && now.After(item.expiresAt) {
			s.sizeBytes -= item.size
			delete(s.items, key)
		}
	}
}

// dropExpired removes key if it has expired, re-checking under the write lock.
func (s *memoryStore[T]) dropExpired(key string) {
	s.mu.Lock()
	if item, ok := s.items[key]; ok && s.expiredLocked(item, time.Now()) {
		s.sizeBytes -= item.size
		delete(s.items, key)
	}
	s.mu.Unlock()
}

// touch refreshes accessedAt for key on a 1-in-8 sample so that read paths do
// not take the write lock on every hit. Approximate LRU ordering is enough.
func (s *memoryStore[T]) touch(key string) {
	if s.accessCount.Add(1)%8 == 0 {
		s.mu.Lock()
		if item, ok := s.items[key]; ok {
			item.accessedAt = time.Now()
			s.items[key] = item
		}
		s.mu.Unlock()
	}
}

// stats prunes expired entries and reports the live entry count and size.
func (s *memoryStore[T]) stats() (int64, int64) {
	s.mu.Lock()
	s.pruneExpiredLocked(time.Now())
	count := int64(len(s.items))
	size := s.sizeBytes
	s.mu.Unlock()
	return count, size
}

// clear empties the store and resets the size accounting.
func (s *memoryStore[T]) clear() {
	s.mu.Lock()
	s.items = make(map[string]memoryItem[T])
	s.sizeBytes = 0
	s.mu.Unlock()
}
