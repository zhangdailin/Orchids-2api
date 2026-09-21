package tokencache

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache implements the Cache interface using Redis as the backend.
// Each key is stored as a simple string value with a Redis TTL for automatic expiry.
type RedisCache struct {
	client *redis.Client
	prefix string
	mu     sync.RWMutex
	ttl    time.Duration
}

// NewRedisCache creates a new Redis-backed token cache.
func NewRedisCache(client *redis.Client, prefix string, ttl time.Duration) *RedisCache {
	if ttl < 0 {
		ttl = 0
	}
	return &RedisCache{
		client: client,
		prefix: prefix + "tcache:",
		ttl:    ttl,
	}
}

func (c *RedisCache) key(k string) string {
	return c.prefix + k
}

func (c *RedisCache) Get(ctx context.Context, key string) (int, bool) {
	if c == nil || c.client == nil {
		return 0, false
	}
	val, err := c.client.Get(ctx, c.key(key)).Result()
	if err != nil {
		return 0, false
	}
	tokens, err := strconv.Atoi(val)
	return tokens, err == nil
}

func (c *RedisCache) Put(ctx context.Context, key string, tokens int) {
	if c == nil || c.client == nil {
		return
	}
	c.mu.RLock()
	ttl := c.ttl
	c.mu.RUnlock()

	// Cache writes are best-effort.
	_ = c.client.Set(ctx, c.key(key), strconv.Itoa(tokens), ttl).Err()
}

func (c *RedisCache) GetStats(ctx context.Context) (int64, int64, error) {
	if c == nil || c.client == nil {
		return 0, 0, nil
	}
	var count int64
	var sizeBytes int64
	var cursor uint64
	pattern := c.prefix + "*"

	for {
		keys, nextCursor, err := c.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return 0, 0, fmt.Errorf("scan failed: %w", err)
		}
		count += int64(len(keys))
		for _, k := range keys {
			sizeBytes += int64(len(k)) + 8 // key length + estimated value size
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return count, sizeBytes, nil
}

func (c *RedisCache) Clear(ctx context.Context) error {
	if c == nil || c.client == nil {
		return nil
	}
	var cursor uint64
	pattern := c.prefix + "*"
	for {
		keys, nextCursor, err := c.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("scan failed: %w", err)
		}
		if len(keys) > 0 {
			// UNLINK removes keys asynchronously, avoiding a large synchronous
			// delete from blocking Redis. Batch the commands in one pipeline so
			// clearing a populated cache does not incur one round trip per key.
			_, err := c.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Unlink(ctx, keys...)
				return nil
			})
			if err != nil {
				return fmt.Errorf("unlink failed: %w", err)
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

func (c *RedisCache) SetTTL(ttl time.Duration) {
	if c == nil {
		return
	}
	if ttl < 0 {
		ttl = 0
	}
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
	// New keys will use the updated TTL; existing keys retain their original expiry.
}
