package summarycache

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/prompt"
)

type RedisCache struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

func NewRedisCache(addr, password string, db int, ttl time.Duration, prefix string) *RedisCache {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	if prefix == "" {
		prefix = "orchids:summary:"
	}

	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	return &RedisCache{
		client: client,
		ttl:    ttl,
		prefix: prefix,
	}
}

func (c *RedisCache) Get(key string) (prompt.SummaryCacheEntry, bool) {
	if c == nil || c.client == nil {
		return prompt.SummaryCacheEntry{}, false
	}

	ctx := context.Background()
	value, err := c.client.Get(ctx, c.prefix+key).Result()
	if err == redis.Nil || err != nil {
		return prompt.SummaryCacheEntry{}, false
	}

	var entry prompt.SummaryCacheEntry
	if err := json.Unmarshal([]byte(value), &entry); err != nil {
		return prompt.SummaryCacheEntry{}, false
	}
	return entry, true
}

func (c *RedisCache) Put(key string, entry prompt.SummaryCacheEntry) {
	if c == nil || c.client == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	ctx := context.Background()
	if c.ttl > 0 {
		_ = c.client.Set(ctx, c.prefix+key, data, c.ttl).Err()
		return
	}
	_ = c.client.Set(ctx, c.prefix+key, data, 0).Err()
}
