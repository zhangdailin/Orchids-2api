package loadbalancer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"

	"golang.org/x/sync/singleflight"
)

const defaultCacheTTL = 5 * time.Second

type LoadBalancer struct {
	Store          *store.Store
	mu             sync.RWMutex
	cachedAccounts []*store.Account
	cacheExpires   time.Time
	cacheTTL       time.Duration
	activeConns    sync.Map // map[int64]*atomic.Int64
	sfGroup        singleflight.Group
}

func NewWithCacheTTL(s *store.Store, cacheTTL time.Duration) *LoadBalancer {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &LoadBalancer{
		Store:    s,
		cacheTTL: cacheTTL,
	}
}

func (lb *LoadBalancer) GetModelChannel(ctx context.Context, modelID string) string {
	if lb.Store == nil {
		return ""
	}
	m, err := lb.Store.GetModelByModelID(ctx, modelID)
	if err != nil || m == nil {
		return ""
	}
	return m.Channel
}

func (lb *LoadBalancer) GetNextAccountExcludingByChannel(ctx context.Context, excludeIDs []int64, channel string) (*store.Account, error) {
	accounts, err := lb.getEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}

	var filtered []*store.Account
	excludeSet := make(map[int64]bool)
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	for _, acc := range accounts {
		if excludeSet[acc.ID] {
			continue
		}
		if !lb.isAccountAvailable(ctx, acc) {
			continue
		}
		if channel != "" {
			accType := acc.AccountType
			if strings.TrimSpace(accType) == "" {
				accType = "orchids"
			}
			if !strings.EqualFold(accType, channel) && !strings.EqualFold(acc.AgentMode, channel) {
				continue
			}
		}
		filtered = append(filtered, acc)
	}
	accounts = filtered

	if len(accounts) == 0 {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s", channel)
	}

	account := lb.selectAccount(accounts)

	slog.Info("Selected account", "name", account.Name, "email", account.Email, "session", auth.MaskSensitive(account.SessionID))

	if err := lb.Store.IncrementRequestCount(ctx, account.ID); err != nil {
		return nil, err
	}

	return account, nil
}

// deepCopyAccounts 深拷贝账号切片，避免并发请求共享同一指针导致数据竞争
func deepCopyAccounts(src []*store.Account) []*store.Account {
	dst := make([]*store.Account, len(src))
	for i, acc := range src {
		copied := *acc
		dst[i] = &copied
	}
	return dst
}

func (lb *LoadBalancer) getEnabledAccounts(ctx context.Context) ([]*store.Account, error) {
	now := time.Now()

	// Use singleflight to prevent cache stampede
	val, err, _ := lb.sfGroup.Do("getEnabledAccounts", func() (interface{}, error) {
		lb.mu.RLock()
		if len(lb.cachedAccounts) > 0 && now.Before(lb.cacheExpires) {
			accounts := deepCopyAccounts(lb.cachedAccounts)
			lb.mu.RUnlock()
			return accounts, nil
		}
		lb.mu.RUnlock()

		accounts, err := lb.Store.GetEnabledAccounts(ctx)
		if err != nil {
			return nil, err
		}

		lb.mu.Lock()
		lb.cachedAccounts = accounts
		lb.cacheExpires = time.Now().Add(lb.cacheTTL)
		lb.mu.Unlock()

		return deepCopyAccounts(accounts), nil
	})

	if err != nil {
		return nil, err
	}
	return val.([]*store.Account), nil
}

func (lb *LoadBalancer) selectAccount(accounts []*store.Account) *store.Account {
	if len(accounts) == 0 {
		return nil
	}
	if len(accounts) == 1 {
		return accounts[0]
	}

	var bestAccounts []*store.Account
	minScore := float64(-1)

	for _, acc := range accounts {
		weight := acc.Weight
		if weight <= 0 {
			weight = 1
		}

		var conns int64
		if val, ok := lb.activeConns.Load(acc.ID); ok {
			conns = val.(*atomic.Int64).Load()
		}
		score := float64(conns) / float64(weight)

		if bestAccounts == nil || score < minScore {
			bestAccounts = []*store.Account{acc}
			minScore = score
		} else if score == minScore {
			bestAccounts = append(bestAccounts, acc)
		}
	}

	if len(bestAccounts) > 0 {
		// Randomly select one from the best accounts to ensure load balancing
		return bestAccounts[rand.IntN(len(bestAccounts))]
	}
	return accounts[0]
}

func (lb *LoadBalancer) AcquireConnection(accountID int64) {
	val, _ := lb.activeConns.LoadOrStore(accountID, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

func (lb *LoadBalancer) ReleaseConnection(accountID int64) {
	if val, ok := lb.activeConns.Load(accountID); ok {
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

const (
	// 401 冷却时间：token 可能已刷新，较短间隔后重试
	retry401Default = 5 * time.Minute
	// 403/404 冷却时间：账号可能被封禁或配置错误，较长间隔后重试
	retry403Default = 24 * time.Hour
)

func (lb *LoadBalancer) isAccountAvailable(ctx context.Context, acc *store.Account) bool {
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return true
	}

	now := time.Now()
	switch status {
	case "401":
		// 401 表示 token 过期或会话失效，短时间冷却后自动恢复尝试
		if acc.LastAttempt.IsZero() {
			return false
		}
		if now.Sub(acc.LastAttempt) >= retry401Default {
			lb.clearAccountStatus(ctx, acc, "401 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "403", "404":
		// 403/404 可能是临时封禁或配置问题，较长冷却后自动恢复
		if acc.LastAttempt.IsZero() {
			return false
		}
		if now.Sub(acc.LastAttempt) >= retry403Default {
			lb.clearAccountStatus(ctx, acc, status+" 冷却完成，自动恢复尝试")
			return true
		}
		return false
	default:
		// Unknown status codes are treated as transient errors with a short cooldown
		// to prevent permanent account exclusion.
		if acc.LastAttempt.IsZero() {
			return false
		}
		if now.Sub(acc.LastAttempt) >= retry401Default {
			lb.clearAccountStatus(ctx, acc, status+" 未知状态冷却完成，自动恢复尝试")
			return true
		}
		return false
	}
}

func (lb *LoadBalancer) clearAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	// 清除 token 缓存，防止恢复后仍使用失效的旧 token
	if acc.SessionID != "" {
		orchids.InvalidateCachedToken(acc.SessionID)
	}
	// 清除 warp session 缓存，确保恢复后使用新 token
	if strings.EqualFold(acc.AccountType, "warp") && acc.ID > 0 {
		warp.InvalidateSession(acc.ID)
	}
	lb.mu.Lock()
	acc.StatusCode = ""
	acc.LastAttempt = time.Time{}
	acc.QuotaResetAt = time.Time{}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, reason)
}

// MarkAccountStatus 标记账号状态（供后台刷新等外部调用使用）。
func (lb *LoadBalancer) MarkAccountStatus(ctx context.Context, acc *store.Account, status string) {
	if acc == nil || lb.Store == nil || status == "" {
		return
	}
	lb.mu.Lock()
	if acc.StatusCode == status {
		lb.mu.Unlock()
		return
	}
	acc.StatusCode = status
	acc.LastAttempt = time.Now()
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, "后台刷新失败: "+status)
}

func (lb *LoadBalancer) persistAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	if lb.Store == nil {
		return
	}
	if err := lb.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("账号状态更新失败", "account_id", acc.ID, "reason", reason, "error", err)
		return
	}
	slog.Info("账号状态已更新", "account_id", acc.ID, "status", acc.StatusCode, "reason", reason)
}
