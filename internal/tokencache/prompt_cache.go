package tokencache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
)

type PromptCache interface {
	CheckPromptCache(strategy string, systemTokens, toolsTokens int, systemText, toolsText string) (readTokens int, creationTokens int)
	GetStats(ctx context.Context) (int64, int64, error)
	Clear(ctx context.Context) error
	SetTTL(ttl time.Duration)
}

type MemoryPromptCache struct {
	mu          sync.RWMutex
	ttl         time.Duration
	maxEntries  int
	items       map[string]promptCacheItem
	sizeBytes   int64
	done        chan struct{}
	accessCount atomic.Uint64
}

type promptCacheItem struct {
	tokens     int
	expiresAt  time.Time
	accessedAt time.Time
	size       int64
}

func NewMemoryPromptCache(ttl time.Duration, maxEntries ...int) *MemoryPromptCache {
	if ttl < 0 {
		ttl = 0
	}
	max := 0
	if len(maxEntries) > 0 && maxEntries[0] > 0 {
		max = maxEntries[0]
	}
	c := &MemoryPromptCache{
		ttl:        ttl,
		maxEntries: max,
		items:      make(map[string]promptCacheItem),
		done:       make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *MemoryPromptCache) cleanupLoop() {
	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			c.pruneExpiredLocked(time.Now())
			c.mu.Unlock()
		}
	}
}

func (c *MemoryPromptCache) SetTTL(ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl < 0 {
		ttl = 0
	}
	c.mu.Lock()
	if c.ttl != ttl {
		c.ttl = ttl
		c.items = make(map[string]promptCacheItem)
		c.sizeBytes = 0
	}
	c.mu.Unlock()
}

func (c *MemoryPromptCache) CheckPromptCache(strategy string, systemTokens, toolsTokens int, systemText, toolsText string) (readTokens int, creationTokens int) {
	if c == nil {
		return 0, systemTokens + toolsTokens
	}
	
	// Strategy parsing
	// 0: System + Tools together
	// 1: Split (System and Tools separate)
	// 2: System only
	// 3: Tools only
	readTokens = 0
	creationTokens = 0

	now := time.Now()
	expiresAt := time.Time{}
	if c.ttl > 0 {
		expiresAt = now.Add(c.ttl)
	}

	checkCache := func(key string, tokens int) (int, int) {
		if tokens <= 0 || key == "" {
			return 0, 0
		}
		
		c.mu.RLock()
		item, ok := c.items[key]
		if ok && (c.ttl == 0 || item.expiresAt.IsZero() || !now.After(item.expiresAt)) {
			// Cache Hit
			c.mu.RUnlock()
			if c.accessCount.Add(1)%8 == 0 {
				c.mu.Lock()
				if it, ok := c.items[key]; ok {
					it.accessedAt = time.Now()
					c.items[key] = it
				}
				c.mu.Unlock()
			}
			return tokens, 0
		}
		c.mu.RUnlock()
		
		// Cache Miss - Need to Put
		size := int64(len(key)) + 8
		c.mu.Lock()
		if existing, ok := c.items[key]; ok {
			c.sizeBytes -= existing.size
		} else if c.maxEntries > 0 && len(c.items) >= c.maxEntries {
			c.evictLRULocked()
		}
		c.items[key] = promptCacheItem{
			tokens:     tokens,
			expiresAt:  expiresAt,
			accessedAt: now,
			size:       size,
		}
		c.sizeBytes += size
		c.mu.Unlock()
		
		return 0, tokens
	}

	hash := func(text string) string {
		if len(text) == 0 {
			return ""
		}
		h := sha256.New()
		h.Write([]byte(text))
		return hex.EncodeToString(h.Sum(nil))
	}

	switch strategy {
	case "0": // Together
		key := hash("sys:" + systemText + "|tools:" + toolsText)
		r, cr := checkCache(key, systemTokens+toolsTokens)
		readTokens += r
		creationTokens += cr
	case "1": // Split
		if len(systemText) > 0 && systemTokens > 0 {
			key1 := hash("sys:" + systemText)
			r, cr := checkCache(key1, systemTokens)
			readTokens += r
			creationTokens += cr
		}
		if len(toolsText) > 0 && toolsTokens > 0 {
			key2 := hash("tools:" + toolsText)
			r, cr := checkCache(key2, toolsTokens)
			readTokens += r
			creationTokens += cr
		}
	case "2": // System only
		if len(systemText) > 0 && systemTokens > 0 {
			key := hash("sys:" + systemText)
			r, cr := checkCache(key, systemTokens)
			readTokens += r
			creationTokens += cr
		}
		creationTokens += toolsTokens // Untracked
	case "3": // Tools only
		if len(toolsText) > 0 && toolsTokens > 0 {
			key := hash("tools:" + toolsText)
			r, cr := checkCache(key, toolsTokens)
			readTokens += r
			creationTokens += cr
		}
		creationTokens += systemTokens // Untracked
	default:
		// Default to Split
		if len(systemText) > 0 && systemTokens > 0 {
			key1 := hash("sys:" + systemText)
			r, cr := checkCache(key1, systemTokens)
			readTokens += r
			creationTokens += cr
		}
		if len(toolsText) > 0 && toolsTokens > 0 {
			key2 := hash("tools:" + toolsText)
			r, cr := checkCache(key2, toolsTokens)
			readTokens += r
			creationTokens += cr
		}
	}

	return readTokens, creationTokens
}

func (c *MemoryPromptCache) evictLRULocked() {
	var lruKey string
	var lruTime time.Time
	first := true
	for k, item := range c.items {
		if first || item.accessedAt.Before(lruTime) {
			lruKey = k
			lruTime = item.accessedAt
			first = false
		}
	}
	if !first {
		c.sizeBytes -= c.items[lruKey].size
		delete(c.items, lruKey)
	}
}

func (c *MemoryPromptCache) GetStats(ctx context.Context) (int64, int64, error) {
	if c == nil {
		return 0, 0, nil
	}
	c.mu.Lock()
	c.pruneExpiredLocked(time.Now())
	count := int64(len(c.items))
	size := c.sizeBytes
	c.mu.Unlock()
	return count, size, nil
}

func (c *MemoryPromptCache) Clear(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.items = make(map[string]promptCacheItem)
	c.sizeBytes = 0
	c.mu.Unlock()
	return nil
}

func (c *MemoryPromptCache) pruneExpiredLocked(now time.Time) {
	if c.ttl <= 0 {
		return
	}
	for key, item := range c.items {
		if !item.expiresAt.IsZero() && now.After(item.expiresAt) {
			c.sizeBytes -= item.size
			delete(c.items, key)
		}
	}
}
