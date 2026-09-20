package tokencache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type PromptCache interface {
	CheckPromptCache(strategy string, systemTokens, toolsTokens int, systemText, toolsText string) (readTokens int, creationTokens int)
	GetStats(ctx context.Context) (int64, int64, error)
	Clear(ctx context.Context) error
	SetTTL(ttl time.Duration)
}

type MemoryPromptCache struct {
	memoryStore[promptCacheItem]
	done chan struct{}
}

// promptCacheItem carries no payload: presence in the map is the cached signal.
type promptCacheItem struct{}

func NewMemoryPromptCache(ttl time.Duration, maxEntries ...int) *MemoryPromptCache {
	c := &MemoryPromptCache{
		memoryStore: newMemoryStore[promptCacheItem](ttl, maxEntries...),
		done:        make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *MemoryPromptCache) cleanupLoop() {
	c.runCleanup(c.done, 5*time.Minute)
}

// Close 停止后台清理 goroutine
func (c *MemoryPromptCache) Close() {
	if c == nil {
		return
	}
	stopCleanup(c.done)
}

func (c *MemoryPromptCache) SetTTL(ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl < 0 {
		ttl = 0
	}
	c.setTTL(ttl)
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
	checkCache := func(key string, tokens int) (int, int) {
		if tokens <= 0 || key == "" {
			return 0, 0
		}

		now := time.Now()
		c.mu.RLock()
		item, ok := c.items[key]
		if ok && !c.expiredLocked(item, now) {
			c.mu.RUnlock()
			c.touch(key)
			return tokens, 0
		}
		c.mu.RUnlock()

		// Cache Miss - Need to Put. Re-check after taking the write lock because
		// another goroutine may have inserted this key in the meantime.
		size := int64(len(key)) + 8
		c.mu.Lock()
		if item, ok := c.items[key]; ok && !c.expiredLocked(item, now) {
			c.mu.Unlock()
			c.touch(key)
			return tokens, 0
		}
		expiresAt := c.expiresAtLocked(now)
		if existing, ok := c.items[key]; ok {
			c.sizeBytes -= existing.size
		} else if c.maxEntries > 0 && len(c.items) >= c.maxEntries {
			c.evictLRULocked()
		}
		c.items[key] = memoryItem[promptCacheItem]{
			expiresAt:  expiresAt,
			accessedAt: now,
			size:       size,
		}
		c.sizeBytes += size
		c.mu.Unlock()

		return 0, tokens
	}

	hash := func(text string) string {
		if text == "" {
			return ""
		}
		h := sha256.Sum256([]byte(text))
		return hex.EncodeToString(h[:])
	}
	checkPart := func(prefix, text string, tokens int) {
		if text == "" || tokens <= 0 {
			return
		}
		read, created := checkCache(hash(prefix+text), tokens)
		readTokens += read
		creationTokens += created
	}

	switch strategy {
	case "0": // Together
		readTokens, creationTokens = checkCache(hash("sys:"+systemText+"|tools:"+toolsText), systemTokens+toolsTokens)
	case "2": // System only
		checkPart("sys:", systemText, systemTokens)
		creationTokens += toolsTokens // Untracked
	case "3": // Tools only
		checkPart("tools:", toolsText, toolsTokens)
		creationTokens += systemTokens // Untracked
	default: // Split
		checkPart("sys:", systemText, systemTokens)
		checkPart("tools:", toolsText, toolsTokens)
	}

	return readTokens, creationTokens
}

func (c *MemoryPromptCache) GetStats(ctx context.Context) (int64, int64, error) {
	if c == nil {
		return 0, 0, nil
	}
	count, size := c.stats()
	return count, size, nil
}

func (c *MemoryPromptCache) Clear(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.clear()
	return nil
}
