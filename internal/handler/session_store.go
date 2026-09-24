package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// WarpToolBinding is enough state to route a tool result back to the exact
// upstream Warp conversation and account that issued it.
type WarpToolBinding struct {
	ConversationID string `json:"conversation_id"`
	AccountID      int64  `json:"account_id"`
	ToolType       string `json:"tool_type,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	ToolInput      string `json:"tool_input,omitempty"`
	TaskContext    string `json:"task_context,omitempty"`
}

// SessionStore abstracts request session state and Warp tool continuations.
type SessionStore interface {
	GetConvID(ctx context.Context, key string) (string, bool)
	SetConvID(ctx context.Context, key, convID string)
	GetAccountID(ctx context.Context, key string) (int64, bool)
	SetAccountID(ctx context.Context, key string, accountID int64)
	GetWarpTaskContext(ctx context.Context, key string) (string, bool)
	SetWarpTaskContext(ctx context.Context, key, taskContext string)
	// Bindings normally use the caller conversation. Clients that omit one use
	// an anonymous capability namespace: possession of the server-issued,
	// unguessable tool-call ID is sufficient to resume that exact call.
	GetWarpToolBinding(ctx context.Context, conversationKey, toolCallID string) (WarpToolBinding, bool)
	SetWarpToolBinding(ctx context.Context, conversationKey, toolCallID string, binding WarpToolBinding)
	DeleteSession(ctx context.Context, key string)
	// Touch refreshes the session TTL. For Redis this issues EXPIRE; for memory it updates lastAccess.
	Touch(ctx context.Context, key string)
	// Cleanup removes expired sessions. No-op for Redis (EXPIRE handles it).
	Cleanup(ctx context.Context)
}

// --- Redis Implementation ---

// RedisSessionStore stores session data as Redis HASHes with automatic TTL.
type RedisSessionStore struct {
	client      *redis.Client
	sessionRoot string
	toolRoot    string
	ttl         time.Duration
}

func NewRedisSessionStore(client *redis.Client, prefix string, ttl time.Duration) *RedisSessionStore {
	return &RedisSessionStore{
		client:      client,
		sessionRoot: prefix + "session:",
		toolRoot:    prefix + "warp-tool:",
		ttl:         ttl,
	}
}

func (s *RedisSessionStore) key(k string) string {
	return s.sessionRoot + k
}

func (s *RedisSessionStore) toolKey(conversationKey, toolCallID string) string {
	conversationKey = warpToolBindingNamespace(conversationKey)
	sum := sha256.Sum256([]byte(conversationKey + "\x00" + toolCallID))
	return s.toolRoot + hex.EncodeToString(sum[:])
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

func (s *RedisSessionStore) GetAccountID(ctx context.Context, key string) (int64, bool) {
	val, err := s.client.HGet(ctx, s.key(key), "account_id").Int64()
	return val, err == nil && val != 0
}

func (s *RedisSessionStore) SetAccountID(ctx context.Context, key string, accountID int64) {
	if accountID == 0 {
		return
	}
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, s.key(key), "account_id", accountID)
	pipe.Expire(ctx, s.key(key), s.ttl)
	_, _ = pipe.Exec(ctx)
}

func (s *RedisSessionStore) GetWarpTaskContext(ctx context.Context, key string) (string, bool) {
	val, err := s.client.HGet(ctx, s.key(key), "warp_task_context").Result()
	return val, err == nil && val != ""
}

func (s *RedisSessionStore) SetWarpTaskContext(ctx context.Context, key, taskContext string) {
	if key == "" || taskContext == "" {
		return
	}
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, s.key(key), "warp_task_context", taskContext)
	pipe.Expire(ctx, s.key(key), s.ttl)
	_, _ = pipe.Exec(ctx)
}

func (s *RedisSessionStore) GetWarpToolBinding(ctx context.Context, conversationKey, toolCallID string) (WarpToolBinding, bool) {
	var binding WarpToolBinding
	if toolCallID == "" {
		return WarpToolBinding{}, false
	}
	raw, err := s.client.Get(ctx, s.toolKey(conversationKey, toolCallID)).Bytes()
	if err != nil || json.Unmarshal(raw, &binding) != nil || binding.ConversationID == "" {
		return WarpToolBinding{}, false
	}
	return binding, true
}

func (s *RedisSessionStore) SetWarpToolBinding(ctx context.Context, conversationKey, toolCallID string, binding WarpToolBinding) {
	if toolCallID == "" || binding.ConversationID == "" {
		return
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return
	}
	_ = s.client.Set(ctx, s.toolKey(conversationKey, toolCallID), raw, s.ttl).Err()
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
	convID      string
	accountID   int64
	taskContext string
	lastAccess  time.Time
}

type memoryWarpToolBinding struct {
	binding    WarpToolBinding
	lastAccess time.Time
}

// MemorySessionStore stores session data in-memory using a sharded map pattern.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*memorySession
	tools    map[string]*memoryWarpToolBinding
	ttl      time.Duration
	maxSize  int
}

func NewMemorySessionStore(ttl time.Duration, maxSize int) *MemorySessionStore {
	return &MemorySessionStore{
		sessions: make(map[string]*memorySession),
		tools:    make(map[string]*memoryWarpToolBinding),
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

func (s *MemorySessionStore) GetAccountID(_ context.Context, key string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[key]
	if !ok || sess.accountID == 0 {
		return 0, false
	}
	return sess.accountID, true
}

func (s *MemorySessionStore) SetAccountID(_ context.Context, key string, accountID int64) {
	if accountID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.getOrCreate(key)
	sess.accountID = accountID
	sess.lastAccess = time.Now()
}

func (s *MemorySessionStore) GetWarpTaskContext(_ context.Context, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[key]
	if !ok || sess.taskContext == "" {
		return "", false
	}
	return sess.taskContext, true
}

func (s *MemorySessionStore) SetWarpTaskContext(_ context.Context, key, taskContext string) {
	if key == "" || taskContext == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.getOrCreate(key)
	sess.taskContext = taskContext
	sess.lastAccess = time.Now()
}

func memoryToolKey(conversationKey, toolCallID string) string {
	return warpToolBindingNamespace(conversationKey) + "\x00" + toolCallID
}

const anonymousWarpToolNamespace = "__anonymous_warp_tool_capability__"

func warpToolBindingNamespace(conversationKey string) string {
	if conversationKey == "" {
		return anonymousWarpToolNamespace
	}
	return conversationKey
}

func (s *MemorySessionStore) GetWarpToolBinding(_ context.Context, conversationKey, toolCallID string) (WarpToolBinding, bool) {
	if toolCallID == "" {
		return WarpToolBinding{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.tools[memoryToolKey(conversationKey, toolCallID)]
	if !ok || entry.binding.ConversationID == "" || time.Since(entry.lastAccess) > s.ttl {
		return WarpToolBinding{}, false
	}
	return entry.binding, true
}

func (s *MemorySessionStore) SetWarpToolBinding(_ context.Context, conversationKey, toolCallID string, binding WarpToolBinding) {
	if toolCallID == "" || binding.ConversationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.maxSize * 8
	if s.maxSize > 0 && len(s.tools) >= limit {
		var oldestKey string
		var oldestTime time.Time
		for key, entry := range s.tools {
			if oldestKey == "" || entry.lastAccess.Before(oldestTime) {
				oldestKey, oldestTime = key, entry.lastAccess
			}
		}
		delete(s.tools, oldestKey)
	}
	s.tools[memoryToolKey(conversationKey, toolCallID)] = &memoryWarpToolBinding{binding: binding, lastAccess: time.Now()}
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
	for key, entry := range s.tools {
		if now.Sub(entry.lastAccess) > s.ttl {
			delete(s.tools, key)
		}
	}
}
