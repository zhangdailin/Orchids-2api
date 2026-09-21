package loadbalancer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConnTracker tracks active connections per account for weighted least-connections selection.
type ConnTracker interface {
	Acquire(accountID int64)
	Release(accountID int64)
	GetCount(accountID int64) int64
	GetCounts(accountIDs []int64) map[int64]int64
}

// LimitedConnTracker atomically reserves a slot when a hard per-account limit
// is configured. It is optional so existing custom trackers remain compatible.
type LimitedConnTracker interface {
	TryAcquire(accountID int64, limit int64) bool
}

// --- Memory Implementation ---

// MemoryConnTracker uses sync.Map with atomic counters (the original implementation).
type MemoryConnTracker struct {
	conns sync.Map // map[int64]*atomic.Int64
}

func NewMemoryConnTracker() *MemoryConnTracker {
	return &MemoryConnTracker{}
}

func (t *MemoryConnTracker) Acquire(accountID int64) {
	val, _ := t.conns.LoadOrStore(accountID, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

func (t *MemoryConnTracker) TryAcquire(accountID int64, limit int64) bool {
	val, _ := t.conns.LoadOrStore(accountID, &atomic.Int64{})
	counter := val.(*atomic.Int64)
	for {
		current := counter.Load()
		if limit > 0 && current >= limit {
			return false
		}
		if counter.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (t *MemoryConnTracker) Release(accountID int64) {
	if val, ok := t.conns.Load(accountID); ok {
		counter := val.(*atomic.Int64)
		for {
			current := counter.Load()
			if current <= 0 {
				break
			}
			if counter.CompareAndSwap(current, current-1) {
				break
			}
		}
	}
}

func (t *MemoryConnTracker) GetCount(accountID int64) int64 {
	if val, ok := t.conns.Load(accountID); ok {
		return val.(*atomic.Int64).Load()
	}
	return 0
}

func (t *MemoryConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	counts := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		counts[id] = t.GetCount(id)
	}
	return counts
}

// --- Redis Implementation ---

// RedisConnTracker uses Redis INCR/DECR for distributed connection counting.
type RedisConnTracker struct {
	client        *redis.Client
	prefix        string
	releaseScript *redis.Script
	acquireScript *redis.Script
	refreshScript *redis.Script
	mu            sync.Mutex
	held          map[int64][]*redisConnLease
	closed        bool
}

const redisConnLeaseTTL = 2 * time.Minute

type redisConnLease struct {
	id   string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func NewRedisConnTracker(client *redis.Client, prefix string) *RedisConnTracker {
	t := &RedisConnTracker{
		client: client,
		prefix: prefix + "conns:",
		held:   make(map[int64][]*redisConnLease),
	}
	// Each request owns one expiring sorted-set member. A crashed process stops
	// renewing its members and Redis reclaims them automatically.
	t.releaseScript = redis.NewScript(`
		return redis.call("ZREM", KEYS[1], ARGV[1])
	`)
	t.acquireScript = redis.NewScript(`
		local key = KEYS[1]
		local limit = tonumber(ARGV[1]) or 0
		local kind = redis.call("TYPE", key).ok
		if kind ~= "none" and kind ~= "zset" then redis.call("DEL", key) end
		redis.call("ZREMRANGEBYSCORE", key, "-inf", ARGV[2])
		local current = redis.call("ZCARD", key)
		if limit > 0 and current >= limit then
			return 0
		end
		redis.call("ZADD", key, ARGV[3], ARGV[4])
		redis.call("PEXPIRE", key, ARGV[5])
		return 1
	`)
	t.refreshScript = redis.NewScript(`
		if redis.call("ZSCORE", KEYS[1], ARGV[1]) == false then return 0 end
		redis.call("ZADD", KEYS[1], "XX", ARGV[2], ARGV[1])
		redis.call("PEXPIRE", KEYS[1], ARGV[3])
		return 1
	`)
	return t
}

func (t *RedisConnTracker) key(accountID int64) string {
	return fmt.Sprintf("%s%d", t.prefix, accountID)
}

func (t *RedisConnTracker) Acquire(accountID int64) {
	_, _ = t.acquire(accountID, 0)
}

func (t *RedisConnTracker) TryAcquire(accountID int64, limit int64) bool {
	_, ok := t.acquire(accountID, limit)
	return ok
}

func (t *RedisConnTracker) Release(accountID int64) {
	if t == nil || t.client == nil || accountID == 0 {
		return
	}
	t.mu.Lock()
	list := t.held[accountID]
	if len(list) == 0 {
		t.mu.Unlock()
		return
	}
	lease := list[len(list)-1]
	list = list[:len(list)-1]
	if len(list) == 0 {
		delete(t.held, accountID)
	} else {
		t.held[accountID] = list
	}
	t.mu.Unlock()
	lease.once.Do(func() { close(lease.stop) })
	<-lease.done
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id).Err()
}

// Close releases every lease owned by this process. Expiry remains the crash
// fallback, but a graceful service restart must not make healthy accounts look
// concurrency-exhausted until the lease TTL elapses.
func (t *RedisConnTracker) Close() {
	if t == nil || t.client == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	held := t.held
	t.held = make(map[int64][]*redisConnLease)
	t.mu.Unlock()

	for _, leases := range held {
		for _, lease := range leases {
			lease.once.Do(func() { close(lease.stop) })
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := t.client.Pipeline()
	for accountID, leases := range held {
		for _, lease := range leases {
			<-lease.done
			pipe.ZRem(ctx, t.key(accountID), lease.id)
		}
	}
	_, _ = pipe.Exec(ctx)
}

func (t *RedisConnTracker) GetCount(accountID int64) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	val, err := redis.NewScript(`
		local kind = redis.call("TYPE", KEYS[1]).ok
		if kind ~= "none" and kind ~= "zset" then redis.call("DEL", KEYS[1]); return 0 end
		redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[1])
		return redis.call("ZCARD", KEYS[1])
	`).Run(ctx, t.client, []string{t.key(accountID)}, time.Now().UnixMilli()).Int64()
	if err != nil {
		return 0
	}
	return val
}

func (t *RedisConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	result := make(map[int64]int64, len(accountIDs))

	if len(accountIDs) == 0 {
		return result
	}
	// Account selection is on every API request. One failing pipeline must not
	// degrade into N sequential two-second Redis calls and amplify an outage by
	// the account-pool size. Use one bounded batch and fail open with zero counts;
	// TryAcquire remains the authoritative atomic limit check.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := t.client.Pipeline()
	now := fmt.Sprint(time.Now().UnixMilli())
	commands := make([]*redis.IntCmd, len(accountIDs))
	for i, id := range accountIDs {
		key := t.key(id)
		pipe.ZRemRangeByScore(ctx, key, "-inf", now)
		commands[i] = pipe.ZCard(ctx, key)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		for _, id := range accountIDs {
			result[id] = 0
		}
		return result
	}

	for i, command := range commands {
		value, _ := command.Result()
		result[accountIDs[i]] = value
	}
	return result
}

func (t *RedisConnTracker) acquire(accountID, limit int64) (*redisConnLease, bool) {
	if t == nil || t.client == nil || accountID == 0 {
		return nil, false
	}
	// A newly started process can briefly race Redis connection establishment or
	// script loading. Retry one time so a transport hiccup is not misreported as
	// a hard account concurrency rejection. A limit rejection (result == 0) is
	// returned immediately and remains subject to the real per-account limit.
	var lease *redisConnLease
	var result int64
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		lease = &redisConnLease{id: newRedisConnLeaseID(), stop: make(chan struct{}), done: make(chan struct{})}
		now := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err = t.acquireScript.Run(ctx, t.client, []string{t.key(accountID)},
			limit, now.UnixMilli(), now.Add(redisConnLeaseTTL).UnixMilli(), lease.id, (redisConnLeaseTTL * 2).Milliseconds()).Int64()
		cancel()
		if err == nil || result == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || result != 1 {
		return nil, false
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id).Err()
		return nil, false
	}
	t.held[accountID] = append(t.held[accountID], lease)
	t.mu.Unlock()
	go t.renew(accountID, lease)
	return lease, true
}

func (t *RedisConnTracker) renew(accountID int64, lease *redisConnLease) {
	defer close(lease.done)
	ticker := time.NewTicker(redisConnLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-lease.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = t.refreshScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id,
				time.Now().Add(redisConnLeaseTTL).UnixMilli(), (redisConnLeaseTTL * 2).Milliseconds()).Err()
			cancel()
		}
	}
}

func newRedisConnLeaseID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("lease-%d", time.Now().UnixNano())
}
