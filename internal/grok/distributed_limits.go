package grok

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisGrokLimits shares pacing and team/model cooldowns across replicas.
type redisGrokLimits struct {
	client *redis.Client
	prefix string
	note   *redis.Script
}

var distributedGrokLimits struct {
	sync.RWMutex
	backend *redisGrokLimits
}

func configureDistributedGrokLimits(client *redis.Client, prefix string) {
	if client == nil {
		return
	}
	backend := &redisGrokLimits{
		client: client,
		prefix: prefix + "grok:limits:",
		note: redis.NewScript(`
			local current = redis.call("PTTL", KEYS[1])
			local wanted = tonumber(ARGV[1])
			if current < wanted then redis.call("PSETEX", KEYS[1], wanted, "1") end
			return 1
		`),
	}
	distributedGrokLimits.Lock()
	distributedGrokLimits.backend = backend
	distributedGrokLimits.Unlock()
}

func grokLimitsBackend() *redisGrokLimits {
	distributedGrokLimits.RLock()
	backend := distributedGrokLimits.backend
	distributedGrokLimits.RUnlock()
	return backend
}

func (b *redisGrokLimits) key(kind string, parts ...string) string {
	hash := sha256.Sum256([]byte(fmt.Sprint(parts)))
	return b.prefix + kind + ":" + hex.EncodeToString(hash[:16])
}

func (b *redisGrokLimits) noteCooldown(scope RateLimitScope, identity, model string, duration time.Duration) error {
	if b == nil || b.client == nil || duration <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return b.note.Run(ctx, b.client, []string{b.key("cooldown", string(scope), identity, model)}, duration.Milliseconds()).Err()
}

func (b *redisGrokLimits) cooldownRemaining(scope RateLimitScope, identity, model string) time.Duration {
	if b == nil || b.client == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	remaining, err := b.client.PTTL(ctx, b.key("cooldown", string(scope), identity, model)).Result()
	if err != nil || remaining <= 0 {
		return 0
	}
	return remaining
}

func (b *redisGrokLimits) waitPacing(ctx context.Context, identity string, rate float64) error {
	if b == nil || b.client == nil || rate <= 0 {
		return nil
	}
	interval := time.Duration(float64(time.Second) / rate)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	key := b.key("pace", identity)
	for {
		acquired, err := b.client.SetNX(ctx, key, "1", interval).Result()
		if err != nil {
			return err
		}
		if acquired {
			return nil
		}
		remaining, err := b.client.PTTL(ctx, key).Result()
		if err != nil || remaining <= 0 {
			remaining = 10 * time.Millisecond
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}
