package tokencache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Cache interface {
	Get(ctx context.Context, key string) (int, bool)
	Put(ctx context.Context, key string, tokens int)
	GetStats(ctx context.Context) (int64, int64, error)
	Clear(ctx context.Context) error
	SetTTL(ttl time.Duration)
}

type MemoryCache struct {
	memoryStore[cacheItem]
	done    chan struct{}
	stopped chan struct{}
}

type cacheItem struct {
	tokens int
}

func NewMemoryCache(ttl time.Duration, maxEntries ...int) *MemoryCache {
	c := &MemoryCache{
		memoryStore: newMemoryStore[cacheItem](ttl, maxEntries...),
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	// Start background cleanup
	go c.cleanupLoop()
	return c
}

func CacheKey(strategy, model, text string) string {
	useModel := normalizeStrategy(strategy) == "split"
	hasher := sha256.New()
	if useModel {
		model = strings.ToLower(strings.TrimSpace(model))
		hasher.Write([]byte(model))
		hasher.Write([]byte{0})
	}
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}

func (c *MemoryCache) SetTTL(ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl < 0 {
		ttl = 0
	}
	c.setTTL(ttl)
}

func (c *MemoryCache) Get(ctx context.Context, key string) (int, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.RLock()
	item, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return 0, false
	}
	if c.expiredLocked(item, time.Now()) {
		c.mu.RUnlock()
		c.dropExpired(key)
		return 0, false
	}
	c.mu.RUnlock()

	// Sampled LRU update: only update accessedAt ~12.5% of the time to avoid
	// write-lock contention on every read. Approximate LRU ordering is
	// sufficient for eviction decisions.
	c.touch(key)

	return item.value.tokens, true
}

func (c *MemoryCache) Put(ctx context.Context, key string, tokens int) {
	if c == nil {
		return
	}
	now := time.Now()
	size := int64(len(key)) + 8
	c.mu.Lock()
	expiresAt := c.expiresAtLocked(now)
	if existing, ok := c.items[key]; ok {
		c.sizeBytes -= existing.size
	} else if c.maxEntries > 0 && len(c.items) >= c.maxEntries {
		c.evictLRULocked()
	}
	c.items[key] = memoryItem[cacheItem]{
		value:      cacheItem{tokens: tokens},
		expiresAt:  expiresAt,
		accessedAt: now,
		size:       size,
	}
	c.sizeBytes += size
	c.mu.Unlock()
}

func (c *MemoryCache) GetStats(ctx context.Context) (int64, int64, error) {
	if c == nil {
		return 0, 0, nil
	}
	count, size := c.stats()
	return count, size, nil
}

func (c *MemoryCache) Clear(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.clear()
	return nil
}

func normalizeStrategy(strategy string) string {
	if strings.EqualFold(strings.TrimSpace(strategy), "split") {
		return "split"
	}
	return "mix"
}
