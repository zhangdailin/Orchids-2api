package loadbalancer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
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
	connTracker    ConnTracker
	sfGroup        singleflight.Group
}

func NewWithCacheTTL(s *store.Store, cacheTTL time.Duration) *LoadBalancer {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &LoadBalancer{
		Store:       s,
		cacheTTL:    cacheTTL,
		connTracker: NewMemoryConnTracker(),
	}
}

// SetConnTracker replaces the default in-memory connection tracker.
func (lb *LoadBalancer) SetConnTracker(ct ConnTracker) {
	lb.connTracker = ct
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
	return lb.GetNextAccountExcludingByChannelWithTracker(ctx, excludeIDs, channel, nil)
}

func (lb *LoadBalancer) GetNextAccountExcludingByChannelWithTracker(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker) (*store.Account, error) {
	accounts, err := lb.getEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}

	var filtered []*store.Account
	excludeSet := make(map[int64]bool)
	channelMatched := 0
	rateLimitedUnavailable := 0
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	for _, acc := range accounts {
		if excludeSet[acc.ID] {
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
		channelMatched++
		if !lb.isAccountAvailable(ctx, acc) {
			if strings.TrimSpace(acc.StatusCode) == "429" {
				rateLimitedUnavailable++
			}
			continue
		}
		filtered = append(filtered, acc)
	}
	accounts = filtered

	if len(accounts) == 0 {
		if channel != "" && channelMatched > 0 && rateLimitedUnavailable == channelMatched {
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are rate-limited or cooling down)", channel)
		}
		return nil, fmt.Errorf("no enabled accounts available for channel: %s", channel)
	}

	account := lb.selectAccountWithTracker(accounts, tracker)

	slog.Debug("Selected account", "id", account.ID, "name", account.Name, "type", account.AccountType, "session", auth.MaskSensitive(account.SessionID))

	return account, nil
}

func (lb *LoadBalancer) getEnabledAccounts(ctx context.Context) ([]*store.Account, error) {
	now := time.Now()

	lb.mu.RLock()
	if len(lb.cachedAccounts) > 0 && now.Before(lb.cacheExpires) {
		accounts := lb.cachedAccounts
		lb.mu.RUnlock()
		return accounts, nil
	}
	lb.mu.RUnlock()

	// Use singleflight to prevent cache stampede
	val, err, _ := lb.sfGroup.Do("getEnabledAccounts", func() (interface{}, error) {
		// Double check after acquiring singleflight lock
		lb.mu.RLock()
		if len(lb.cachedAccounts) > 0 && now.Before(lb.cacheExpires) {
			accounts := lb.cachedAccounts
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

		return accounts, nil
	})

	if err != nil {
		return nil, err
	}
	return val.([]*store.Account), nil
}

func (lb *LoadBalancer) selectAccount(accounts []*store.Account) *store.Account {
	return lb.selectAccountWithTracker(accounts, nil)
}

func (lb *LoadBalancer) selectAccountWithTracker(accounts []*store.Account, tracker ConnTracker) *store.Account {
	if len(accounts) == 0 {
		return nil
	}
	if len(accounts) == 1 {
		return accounts[0]
	}
	if tracker == nil {
		tracker = lb.connTracker
	}
	if tracker == nil {
		tracker = NewMemoryConnTracker()
	}

	// Batch-fetch connection counts
	ids := make([]int64, len(accounts))
	for i, acc := range accounts {
		ids[i] = acc.ID
	}
	connCounts := tracker.GetCounts(ids)

	var bestAccounts []*store.Account
	minScore := float64(-1)

	for _, acc := range accounts {
		weight := acc.Weight
		if weight <= 0 {
			weight = 1
		}

		conns := connCounts[acc.ID]
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
	lb.connTracker.Acquire(accountID)
}

func (lb *LoadBalancer) ReleaseConnection(accountID int64) {
	lb.connTracker.Release(accountID)
}

const (
	// 401 冷却时间：token 可能已刷新，较短间隔后重试
	retry401Default = 5 * time.Minute
	// 402 对 Puter 来说通常表示余额/credits 不足，不应很快重新参与调度
	retry402Default = 6 * time.Hour
	// 429 冷却时间：限流通常是暂时性的，优先等待较短窗口再恢复尝试
	retry429Default = 1 * time.Minute
	// 403/404 冷却时间：账号可能被封禁或配置错误，较长间隔后重试
	retry403Default = 24 * time.Hour
	// Grok 的 403 很多是 Cloudflare challenge/临时风控，不应长时间拉黑
	retry403Grok = 10 * time.Minute
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
	case "429":
		// 429 优先尊重显式 quota reset 时间；没有 reset 信息时走较短冷却。
		if !acc.QuotaResetAt.IsZero() {
			if !now.Before(acc.QuotaResetAt) {
				lb.clearAccountStatus(ctx, acc, "429 冷却完成，自动恢复尝试")
				return true
			}
			return false
		}
		if acc.LastAttempt.IsZero() {
			return false
		}
		if now.Sub(acc.LastAttempt) >= retry429Default {
			lb.clearAccountStatus(ctx, acc, "429 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "402":
		// 402 通常表示余额/credits 不足。若上游给出 reset 时间则优先尊重，
		// 否则使用更长的冷却，避免调度器持续撞到同一个无额度账号。
		if !acc.QuotaResetAt.IsZero() {
			if !now.Before(acc.QuotaResetAt) {
				lb.clearAccountStatus(ctx, acc, "402 冷却完成，自动恢复尝试")
				return true
			}
			return false
		}
		if acc.LastAttempt.IsZero() {
			return false
		}
		if now.Sub(acc.LastAttempt) >= retry402Default {
			lb.clearAccountStatus(ctx, acc, "402 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "403", "404":
		// 403/404 可能是临时封禁或配置问题。
		// 对 Grok 来说，403 很多是 Cloudflare challenge，不应长时间拉黑。
		if acc.LastAttempt.IsZero() {
			return false
		}
		cooldown := retry403Default
		if strings.EqualFold(acc.AccountType, "grok") {
			cooldown = retry403Grok
		}
		if now.Sub(acc.LastAttempt) >= cooldown {
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
	// Find and update the account in the cached slice so the change reflects immediately
	lb.mu.Lock()
	acc.StatusCode = ""
	acc.LastAttempt = time.Time{}
	acc.QuotaResetAt = time.Time{}
	for _, cached := range lb.cachedAccounts {
		if cached.ID == acc.ID {
			cached.StatusCode = ""
			cached.LastAttempt = time.Time{}
			cached.QuotaResetAt = time.Time{}
			break
		}
	}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, reason)
}

// MarkAccountStatus 标记账号状态（供后台刷新等外部调用使用）。
func (lb *LoadBalancer) MarkAccountStatus(ctx context.Context, acc *store.Account, status string) {
	if acc == nil || lb.Store == nil || status == "" {
		return
	}
	lb.mu.Lock()
	now := time.Now()
	acc.StatusCode = status
	acc.LastAttempt = now

	// Ensure the cache is updated as well
	for _, cached := range lb.cachedAccounts {
		if cached.ID == acc.ID {
			cached.StatusCode = status
			cached.LastAttempt = now
			break
		}
	}
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
	slog.Debug("账号状态已更新", "account_id", acc.ID, "status", acc.StatusCode, "reason", reason)
}
