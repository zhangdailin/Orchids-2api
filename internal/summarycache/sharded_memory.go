package summarycache

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"

	"orchids-api/internal/prompt"
)

type ShardedMemoryCache struct {
	shards     []*memoryShard
	shardCount int
}

type memoryShard struct {
	mu         sync.RWMutex
	maxEntries int
	ttl        time.Duration
	ll         *list.List
	items      map[string]*list.Element
}

type shardCacheItem struct {
	key       string
	value     prompt.SummaryCacheEntry
	expiresAt time.Time
}

func NewShardedMemoryCache(maxEntries int, ttl time.Duration, shardCount int) *ShardedMemoryCache {
	if shardCount <= 0 {
		shardCount = 16
	}
	if maxEntries < 0 {
		maxEntries = 0
	}

	entriesPerShard := maxEntries / shardCount
	if entriesPerShard == 0 {
		entriesPerShard = 1
	}

	shards := make([]*memoryShard, shardCount)
	for i := 0; i < shardCount; i++ {
		shards[i] = &memoryShard{
			maxEntries: entriesPerShard,
			ttl:        ttl,
			ll:         list.New(),
			items:      make(map[string]*list.Element),
		}
	}

	return &ShardedMemoryCache{
		shards:     shards,
		shardCount: shardCount,
	}
}

func (c *ShardedMemoryCache) getShard(key string) *memoryShard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return c.shards[h.Sum32()%uint32(c.shardCount)]
}

func (c *ShardedMemoryCache) Get(key string) (prompt.SummaryCacheEntry, bool) {
	shard := c.getShard(key)
	return shard.get(key)
}

func (c *ShardedMemoryCache) Put(key string, entry prompt.SummaryCacheEntry) {
	shard := c.getShard(key)
	shard.put(key, entry)
}

func (s *memoryShard) get(key string) (prompt.SummaryCacheEntry, bool) {
	if s.maxEntries <= 0 {
		return prompt.SummaryCacheEntry{}, false
	}

	s.mu.RLock()
	el, ok := s.items[key]
	if !ok {
		s.mu.RUnlock()
		return prompt.SummaryCacheEntry{}, false
	}

	item := el.Value.(*shardCacheItem)
	if s.ttl > 0 && time.Now().After(item.expiresAt) {
		s.mu.RUnlock()
		s.mu.Lock()
		s.removeElement(el)
		s.mu.Unlock()
		return prompt.SummaryCacheEntry{}, false
	}

	value := item.value
	s.mu.RUnlock()

	s.mu.Lock()
	s.ll.MoveToFront(el)
	s.mu.Unlock()

	return value, true
}

func (s *memoryShard) put(key string, entry prompt.SummaryCacheEntry) {
	if s.maxEntries <= 0 {
		return
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[key]; ok {
		item := el.Value.(*shardCacheItem)
		item.value = entry
		item.expiresAt = s.expiryTime()
		s.ll.MoveToFront(el)
		return
	}

	item := &shardCacheItem{
		key:       key,
		value:     entry,
		expiresAt: s.expiryTime(),
	}
	el := s.ll.PushFront(item)
	s.items[key] = el

	if s.ll.Len() > s.maxEntries {
		s.removeOldest()
	}
}

func (s *memoryShard) expiryTime() time.Time {
	if s.ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(s.ttl)
}

func (s *memoryShard) removeOldest() {
	el := s.ll.Back()
	if el != nil {
		s.removeElement(el)
	}
}

func (s *memoryShard) removeElement(el *list.Element) {
	s.ll.Remove(el)
	item := el.Value.(*shardCacheItem)
	delete(s.items, item.key)
}
