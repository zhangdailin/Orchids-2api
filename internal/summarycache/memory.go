package summarycache

import (
	"container/list"
	"sync"
	"time"

	"orchids-api/internal/prompt"
)

type MemoryCache struct {
	mu         sync.RWMutex
	maxEntries int
	ttl        time.Duration
	ll         *list.List
	items      map[string]*list.Element
}

type cacheItem struct {
	key       string
	value     prompt.SummaryCacheEntry
	expiresAt time.Time
}

func NewMemoryCache(maxEntries int, ttl time.Duration) *MemoryCache {
	if maxEntries < 0 {
		maxEntries = 0
	}
	return &MemoryCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
	}
}

func (c *MemoryCache) Get(key string) (prompt.SummaryCacheEntry, bool) {
	if c == nil || c.maxEntries <= 0 {
		return prompt.SummaryCacheEntry{}, false
	}

	c.mu.RLock()
	el, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return prompt.SummaryCacheEntry{}, false
	}

	item := el.Value.(*cacheItem)
	if c.ttl > 0 && time.Now().After(item.expiresAt) {
		c.mu.RUnlock()
		c.mu.Lock()
		c.removeElement(el)
		c.mu.Unlock()
		return prompt.SummaryCacheEntry{}, false
	}

	c.mu.RUnlock()
	c.mu.Lock()
	c.ll.MoveToFront(el)
	value := item.value
	c.mu.Unlock()

	return value, true
}

func (c *MemoryCache) Put(key string, entry prompt.SummaryCacheEntry) {
	if c == nil || c.maxEntries <= 0 {
		return
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		item := el.Value.(*cacheItem)
		item.value = entry
		item.expiresAt = c.expiryTime()
		c.ll.MoveToFront(el)
		return
	}

	item := &cacheItem{
		key:       key,
		value:     entry,
		expiresAt: c.expiryTime(),
	}
	el := c.ll.PushFront(item)
	c.items[key] = el

	if c.ll.Len() > c.maxEntries {
		c.removeOldest()
	}
}

func (c *MemoryCache) expiryTime() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(c.ttl)
}

func (c *MemoryCache) removeOldest() {
	el := c.ll.Back()
	if el != nil {
		c.removeElement(el)
	}
}

func (c *MemoryCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	item := el.Value.(*cacheItem)
	delete(c.items, item.key)
}
