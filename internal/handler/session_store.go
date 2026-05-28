package handler

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionStore abstracts session state storage (workdir + conversation ID).
type SessionStore interface {
	GetWorkdir(ctx context.Context, key string) (string, bool)
	SetWorkdir(ctx context.Context, key, workdir string)
	GetConvID(ctx context.Context, key string) (string, bool)
	SetConvID(ctx context.Context, key, convID string)
	DeleteSession(ctx context.Context, key string)
	// Touch refreshes the session TTL. For Redis this issues EXPIRE; for memory it updates lastAccess.
	Touch(ctx context.Context, key string)
	// Cleanup removes expired sessions. No-op for Redis (EXPIRE handles it).
	Cleanup(ctx context.Context)
}

// --- Redis Implementation ---

// RedisSessionStore stores session data as Redis HASHes with automatic TTL.
type RedisSessionStore struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

func NewRedisSessionStore(client *redis.Client, prefix string, ttl time.Duration) *RedisSessionStore {
	return &RedisSessionStore{
		client: client,
		prefix: prefix + "session:",
		ttl:    ttl,
	}
}

func (s *RedisSessionStore) key(k string) string {
	return s.prefix + k
}

func (s *RedisSessionStore) GetWorkdir(ctx context.Context, key string) (string, bool) {
	val, err := s.client.HGet(ctx, s.key(key), "workdir").Result()
	if err != nil {
		return "", false
	}
	return val, true
}

func (s *RedisSessionStore) SetWorkdir(ctx context.Context, key, workdir string) {
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, s.key(key), "workdir", workdir)
	pipe.Expire(ctx, s.key(key), s.ttl)
	pipe.Exec(ctx)
}

func (s *RedisSessionStore) GetConvID(ctx context.Context, key string) (string, bool) {
	val, err := s.client.HGet(ctx, s.key(key), "conv_id").Result()
	if err != nil {
		return "", false
	}
	return val, true
}

func (s *RedisSessionStore) SetConvID(ctx context.Context, key, convID string) {
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, s.key(key), "conv_id", convID)
	pipe.Expire(ctx, s.key(key), s.ttl)
	pipe.Exec(ctx)
}

func (s *RedisSessionStore) DeleteSession(ctx context.Context, key string) {
	s.client.Del(ctx, s.key(key))
}

func (s *RedisSessionStore) Touch(ctx context.Context, key string) {
	s.client.Expire(ctx, s.key(key), s.ttl)
}

func (s *RedisSessionStore) Cleanup(_ context.Context) {
	// No-op: Redis EXPIRE handles automatic cleanup.
}

// --- Memory Implementation ---

type memorySession struct {
	workdir    string
	convID     string
	lastAccess time.Time
}

// MemorySessionStore stores session data in-memory using a sharded map pattern.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*memorySession
	ttl      time.Duration
	maxSize  int
}

func NewMemorySessionStore(ttl time.Duration, maxSize int) *MemorySessionStore {
	return &MemorySessionStore{
		sessions: make(map[string]*memorySession),
		ttl:      ttl,
		maxSize:  maxSize,
	}
}

func (s *MemorySessionStore) getOrCreate(key string) *memorySession {
	sess, ok := s.sessions[key]
	if !ok {
		// 容量超限时，驱逐最久未访问的 session
		if s.maxSize > 0 && len(s.sessions) >= s.maxSize {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range s.sessions {
				if oldestKey == "" || v.lastAccess.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastAccess
				}
			}
			if oldestKey != "" {
				delete(s.sessions, oldestKey)
			}
		}
		sess = &memorySession{}
		s.sessions[key] = sess
	}
	return sess
}

func (s *MemorySessionStore) GetWorkdir(_ context.Context, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[key]
	if !ok || sess.workdir == "" {
		return "", false
	}
	return sess.workdir, true
}

func (s *MemorySessionStore) SetWorkdir(_ context.Context, key, workdir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.getOrCreate(key)
	sess.workdir = workdir
	sess.lastAccess = time.Now()
}

func (s *MemorySessionStore) GetConvID(_ context.Context, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[key]
	if !ok || sess.convID == "" {
		return "", false
	}
	return sess.convID, true
}

func (s *MemorySessionStore) SetConvID(_ context.Context, key, convID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.getOrCreate(key)
	sess.convID = convID
	sess.lastAccess = time.Now()
}

func (s *MemorySessionStore) DeleteSession(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}

func (s *MemorySessionStore) Touch(_ context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[key]; ok {
		sess.lastAccess = time.Now()
	}
}

func (s *MemorySessionStore) Cleanup(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, sess := range s.sessions {
		if now.Sub(sess.lastAccess) > s.ttl {
			delete(s.sessions, key)
		}
	}
}
