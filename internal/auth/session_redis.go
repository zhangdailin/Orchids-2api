package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisSessionBackend keeps admin sessions in Redis so they survive a process
// restart (every deploy) and are shared by every replica behind the same
// database. Only a digest of the token is stored, so a Redis dump never
// exposes a usable session cookie.
type redisSessionBackend struct {
	client *redis.Client
	prefix string
}

// NewRedisSessionBackend returns a durable session store, or nil when no Redis
// client is available. Callers keep the in-process store in that case.
func NewRedisSessionBackend(client *redis.Client, prefix string) SessionBackend {
	if client == nil {
		return nil
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "orchids:"
	}
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}
	return &redisSessionBackend{client: client, prefix: prefix}
}

func (b *redisSessionBackend) key(token string) string {
	digest := sha256.Sum256([]byte(token))
	return b.prefix + "admin:sessions:" + hex.EncodeToString(digest[:])
}

func (b *redisSessionBackend) SaveSession(ctx context.Context, token string, expiry time.Time) error {
	if b == nil || b.client == nil {
		return nil
	}
	ttl := time.Until(expiry)
	if ttl <= 0 {
		return nil
	}
	return b.client.Set(ctx, b.key(token), "1", ttl).Err()
}

func (b *redisSessionBackend) HasSession(ctx context.Context, token string) (bool, error) {
	if b == nil || b.client == nil {
		return false, nil
	}
	exists, err := b.client.Exists(ctx, b.key(token)).Result()
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

func (b *redisSessionBackend) DeleteSession(ctx context.Context, token string) {
	if b == nil || b.client == nil {
		return
	}
	_ = b.client.Del(ctx, b.key(token)).Err()
}
