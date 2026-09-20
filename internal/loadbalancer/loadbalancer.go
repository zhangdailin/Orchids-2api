package loadbalancer

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/auth"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"

	"golang.org/x/sync/singleflight"
)

const defaultCacheTTL = 5 * time.Second

// DefaultAccountConcurrency is the per-account in-flight ceiling every provider
// falls back to when the account carries no explicit max_concurrent. A single
// provider slot is too narrow for a gateway that multiplexes a chat turn with
// its client's side calls: with one slot the second concurrent request waits out
// the whole reservation window and then fails as an overload, even though the
// upstream account is healthy. Providers used to differ here (WorkBuddy 3, the
// others 1, Qoder unlimited); they now share one explicit value so the pool's
// capacity is a deliberate setting rather than a per-channel accident.
const DefaultAccountConcurrency int64 = 10

// EffectiveAccountConcurrencyLimit resolves the in-flight ceiling for one
// account. An explicit per-account value always wins; a known provider falls
// back to DefaultAccountConcurrency; an unknown account type stays unlimited so
// a future channel is not silently throttled before it has a documented limit.
func EffectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	if acc == nil {
		return 0
	}
	if acc.MaxConcurrent > 0 {
		return int64(acc.MaxConcurrent)
	}
	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "warp", "puter", "workbuddy", "qoder", "cline", "grok":
		return DefaultAccountConcurrency
	default:
		return 0
	}
}

type LoadBalancer struct {
	Store          *store.Store
	mu             sync.RWMutex
	cachedAccounts []*store.Account
	cacheExpires   time.Time
	cacheTTL       time.Duration
	connTracker    ConnTracker
	sfGroup        singleflight.Group
	// scanCursor rotates the window a large pool is examined through.
	scanCursor int
	// lastSelected remembers when each account was last handed out, for the
	// least-recently-used tie-break. It is kept here rather than on the account
	// because the cached account objects are shared by concurrent callers.
	lastSelected map[int64]time.Time
	selectedMu   sync.Mutex
}

func NewWithCacheTTL(s *store.Store, cacheTTL time.Duration) *LoadBalancer {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &LoadBalancer{
		Store:        s,
		cacheTTL:     cacheTTL,
		connTracker:  NewMemoryConnTracker(),
		lastSelected: make(map[int64]time.Time),
	}
}

// accountScanWindow bounds how many accounts one selection examines before it
// falls back to the whole pool.
const accountScanWindow = 64

// rotateScanCursor advances the window start so successive requests cover the
// pool instead of always looking at its head.
func (lb *LoadBalancer) rotateScanCursor(size int) int {
	if size <= 0 {
		return 0
	}
	lb.selectedMu.Lock()
	defer lb.selectedMu.Unlock()
	start := lb.scanCursor % size
	lb.scanCursor = (start + accountScanWindow) % size
	return start
}

// SetConnTracker replaces the default in-memory connection tracker.
func (lb *LoadBalancer) SetConnTracker(ct ConnTracker) {
	lb.connTracker = ct
}

func (lb *LoadBalancer) GetNextAccountExcludingByChannelWithTracker(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker) (*store.Account, error) {
	return lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, channel, tracker, nil)
}

func (lb *LoadBalancer) GetNextAccountExcludingByChannelWithTrackerFilter(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker, filter func(*store.Account) bool) (*store.Account, error) {
	accounts, err := lb.getEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}

	var filtered []*store.Account
	excludeSet := make(map[int64]bool)
	// Counted per scan pass (see scan below), so the reasons reported for an empty
	// pool describe one pass rather than a window and its fallback added together.
	channelCandidates := 0
	channelMatched := 0
	rateLimitedUnavailable := 0
	allowanceParked := 0
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	// A large pool is examined in rotating windows rather than in full. Every
	// request paying for a scan and availability check of thousands of accounts
	// is what made the pool expensive to grow; the window wraps, so every
	// account is still reachable, and a window that yields nothing falls back to
	// the full list so correctness never depends on the window size.
	scanned := accounts
	if len(accounts) > accountScanWindow {
		start := lb.rotateScanCursor(len(accounts))
		scanned = make([]*store.Account, 0, accountScanWindow)
		for offset := 0; offset < accountScanWindow; offset++ {
			scanned = append(scanned, accounts[(start+offset)%len(accounts)])
		}
	}

	scan := func(candidates []*store.Account) bool {
		// Reset per pass: the whole-pool fallback re-scans accounts the window
		// already counted, and doubling the counters would let one pool look like
		// two (and flip which reason is reported for an empty one).
		channelCandidates, channelMatched = 0, 0
		rateLimitedUnavailable, allowanceParked = 0, 0
		for _, acc := range candidates {
			if excludeSet[acc.ID] {
				continue
			}
			if channel != "" {
				accType := acc.AccountType
				if strings.TrimSpace(accType) == "" {
					continue
				}
				if !strings.EqualFold(accType, channel) && !strings.EqualFold(acc.AgentMode, channel) {
					continue
				}
			}
			// Counted before the caller's filter: the difference between "this
			// channel has no accounts" and "this channel has accounts and the
			// request's model filter rejected all of them" is the whole reason the
			// pool is empty, and it is reported below.
			channelCandidates++
			if filter != nil && !filter(acc) {
				continue
			}
			channelMatched++
			if !lb.isAccountAvailable(ctx, acc) {
				switch strings.TrimSpace(acc.StatusCode) {
				case "429":
					rateLimitedUnavailable++
				case "402":
					allowanceParked++
				}
				continue
			}
			filtered = append(filtered, acc)
		}
		return len(filtered) > 0
	}
	if !scan(scanned) && len(scanned) != len(accounts) {
		// Nothing usable in this window: fall back to the whole pool once.
		scan(accounts)
	}
	accounts = filtered

	if len(accounts) == 0 {
		// An empty pool has three different causes and they need three different
		// answers: a request whose model is cooling down on every account, a pool
		// that is rate-limited, and a pool whose allowance is spent. All three used
		// to arrive as the bare sentence below, so "every account is cooling down
		// for this model" reached the operator as "no enabled accounts available
		// for channel", which reads like the channel has no accounts at all — and
		// the caller answered it with a 503 instead of a retryable 429.
		switch {
		case channel != "" && channelCandidates > 0 && channelMatched == 0:
			// The caller's filter (a per-model cooldown) rejected every candidate.
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are cooling down for the requested model)", channel)
		case channel != "" && channelMatched > 0 && rateLimitedUnavailable == channelMatched:
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are rate-limited or cooling down)", channel)
		case channel != "" && channelMatched > 0 && allowanceParked == channelMatched:
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts have exhausted their allowance)", channel)
		}
		return nil, fmt.Errorf("no enabled accounts available for channel: %s", channel)
	}

	account := lb.selectAccountWithTracker(accounts, tracker)
	if account == nil {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", channel)
	}

	slog.Debug("Selected account", "id", account.ID, "name", account.Name, "type", account.AccountType, "session", auth.MaskSensitive(account.SessionID))

	return account, nil
}

// InvalidateAccounts drops the cached snapshot entries for the given accounts so
// the next selection reads their current state.
//
// This replaces the old "wait for the five-second TTL" behaviour: a change that
// has been persisted must be visible to the next request, not to the request
// after next. The whole snapshot is re-read on the next miss, so a removed
// account cannot linger in the pool.
func (lb *LoadBalancer) InvalidateAccounts(ids []int64) {
	if lb == nil || len(ids) == 0 {
		return
	}
	changed := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id != 0 {
			changed[id] = struct{}{}
		}
	}
	if len(changed) == 0 {
		return
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()
	// A change to the pool always invalidates the TTL as well: keeping the deadline
	// would let a later read serve the pre-change snapshot from a slice another
	// caller still holds.
	lb.cacheExpires = time.Time{}
	if len(lb.cachedAccounts) == 0 {
		return
	}
	kept := make([]*store.Account, 0, len(lb.cachedAccounts))
	for _, acc := range lb.cachedAccounts {
		if acc == nil {
			continue
		}
		if _, dirty := changed[acc.ID]; dirty {
			continue
		}
		kept = append(kept, acc)
	}
	lb.cachedAccounts = kept
}

// AccountChanges implements the account-change subscriber contract.
func (lb *LoadBalancer) AccountChanges(ids []int64) { lb.InvalidateAccounts(ids) }

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

func (lb *LoadBalancer) selectAccountWithTracker(accounts []*store.Account, tracker ConnTracker) *store.Account {
	if len(accounts) == 0 {
		return nil
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
		if limit := EffectiveAccountConcurrencyLimit(acc); limit > 0 && conns >= limit {
			continue
		}
		score := float64(conns) / float64(weight)

		if bestAccounts == nil || score < minScore {
			bestAccounts = []*store.Account{acc}
			minScore = score
		} else if score == minScore {
			bestAccounts = append(bestAccounts, acc)
		}
	}
	if len(bestAccounts) == 0 {
		return nil
	}

	// Among equally loaded accounts prefer the one that has been idle longest.
	// Random tie-breaking kept selecting the same low-latency accounts while
	// others were never tried, which shows up as a hot subset in the pool.
	// Accounts never handed out sort first, so a fresh account is tried.
	lb.selectedMu.Lock()
	defer lb.selectedMu.Unlock()
	if lb.lastSelected == nil {
		// A LoadBalancer built as a struct literal (tests, embedders) has no map.
		lb.lastSelected = make(map[int64]time.Time)
	}
	var unseen, coldest []*store.Account
	var oldest time.Time
	for _, acc := range bestAccounts {
		last, seen := lb.lastSelected[acc.ID]
		if !seen {
			unseen = append(unseen, acc)
			continue
		}
		switch {
		case coldest == nil || last.Before(oldest):
			oldest = last
			coldest = []*store.Account{acc}
		case last.Equal(oldest):
			coldest = append(coldest, acc)
		}
	}
	pool := unseen
	if len(pool) == 0 {
		pool = coldest
	}
	if len(pool) == 0 {
		pool = bestAccounts
	}
	picked := pool[rand.IntN(len(pool))]
	lb.lastSelected[picked.ID] = time.Now()
	return picked
}

func (lb *LoadBalancer) AcquireConnection(accountID int64) {
	lb.connTracker.Acquire(accountID)
}

func (lb *LoadBalancer) ReleaseConnection(accountID int64) {
	lb.connTracker.Release(accountID)
}

const (
	// The account-state policy owns these windows; the aliases keep the pool's
	// existing call sites and tests readable while giving every other entrance
	// (scheduler, admin API, account table) the same numbers.
	//
	// 401 冷却时间：token 可能已刷新，较短间隔后重试
	retry401Default = accountpolicy.CooldownAuth
	// 402 对 Puter 来说通常表示余额/credits 不足。Puter 暂无稳定额度/重置时间接口，
	// 默认按日冷却，避免无额度账号反复撞上游。
	retry402Default = accountpolicy.CooldownPayment
	// Puter 的路由额度可能在短窗口内恢复，且当前错误不提供 reset 时间。
	// 每 15 分钟允许一次探测，在避免请求风暴的同时防止整个通道停用一天。
	retry402Puter = accountpolicy.CooldownPuterQuota
	// 429 冷却时间：限流通常是暂时性的，优先等待较短窗口再恢复尝试
	retry429Default = accountpolicy.CooldownRateLimit
	// 403/404 冷却时间：账号可能被封禁或配置错误，较长间隔后重试
	retry403Default = accountpolicy.CooldownBlocked
	// Grok 的 403 很多是 Cloudflare challenge/临时风控，不应长时间拉黑
	retry403Grok = accountpolicy.CooldownBlockedGro
)

func (lb *LoadBalancer) isAccountAvailable(ctx context.Context, acc *store.Account) bool {
	if !store.AccountAuthActive(acc) {
		return false
	}
	// A paid Build billing snapshot is an authoritative routing signal: do not
	// keep probing an account known to be exhausted before the period resets.
	if isPaidGrokBuildAccount(acc) && acc.GrokBilling.IsExhausted() {
		now := time.Now().UTC()
		periodEnd := acc.GrokBilling.PeriodEnd()
		if periodEnd.IsZero() || now.Before(periodEnd) {
			return false
		}
		// Once the billing period ends, admit exactly one account probe per
		// bounded interval. Without an atomic claim every concurrent request
		// floods the same exhausted account the instant PeriodEnd passes.
		if lb.Store == nil {
			return false
		}
		claimed, err := lb.Store.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
		if err != nil || !claimed {
			return false
		}
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return true
	}

	now := time.Now()
	switch status {
	case store.AccountStatusWarpQuotaExhausted:
		// Warp credit exhaustion is a capability downgrade, not an account-wide
		// cooldown. Account/model filters restrict this account to free-only
		// models and reject tools/cloud-agent requests. Once refreshed quota is
		// observed, remove the durable downgrade marker.
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
			return false
		}
		hasRefreshedQuota := acc.WarpMonthlyRemaining+acc.WarpBonusRemaining > 0 ||
			(acc.WarpMonthlyLimit <= 0 && acc.UsageLimit > 0 && acc.UsageCurrent < acc.UsageLimit)
		if hasRefreshedQuota {
			lb.clearAccountStatus(ctx, acc, "Warp 额度已刷新，恢复完整能力")
		}
		return true
	case "401":
		// A refused credential needs operator re-authentication. Legacy rows that
		// only carry StatusCode=401 are also kept out rather than automatically
		// retried with the same dead credential.
		return false
	case "429":
		if acc.LastAttempt.IsZero() {
			return false
		}
		if !accountpolicy.AccountHeld(acc, now) {
			lb.clearAccountStatus(ctx, acc, "429 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "402":
		// Paid Build exhaustion recovers at the billing period boundary rather
		// than after an arbitrary 24-hour probe window.
		if isPaidGrokBuildAccount(acc) {
			if periodEnd := acc.GrokBilling.PeriodEnd(); !periodEnd.IsZero() {
				if !now.Before(periodEnd) {
					lb.clearAccountStatus(ctx, acc, "402 账期已结束，恢复尝试")
					return true
				}
				return false
			}
		}
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
		cooldown := retry402Default
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), "puter") {
			cooldown = retry402Puter
		}
		if now.Sub(acc.LastAttempt) >= cooldown {
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

func isPaidGrokBuildAccount(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(acc.GrokProvider))
	credential := strings.ToLower(strings.TrimSpace(acc.CredentialType))
	if provider != "build" && !(provider == "" && credential == "oauth") {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(acc.Subscription))
	for _, paid := range []string{"super", "pro", "heavy", "lite", "x_basic", "xbasic", "x_premium", "xpremium", "paid", "team", "enterprise"} {
		if strings.Contains(plan, paid) {
			return true
		}
	}
	return false
}

func (lb *LoadBalancer) clearAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	// 清除 warp session 缓存，确保恢复后使用新 token
	if strings.EqualFold(acc.AccountType, "warp") && acc.ID > 0 {
		warp.InvalidateSession(acc.ID)
	}
	// Find and update the account in the cached slice so the change reflects immediately
	lb.mu.Lock()
	acc.StatusCode = ""
	acc.StatusMessage = ""
	acc.LastAttempt = time.Time{}
	acc.QuotaResetAt = time.Time{}
	acc.RateLimitFailures = 0
	for _, cached := range lb.cachedAccounts {
		if cached.ID == acc.ID {
			cached.StatusCode = ""
			cached.StatusMessage = ""
			cached.LastAttempt = time.Time{}
			cached.QuotaResetAt = time.Time{}
			cached.RateLimitFailures = 0
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
	if status != "429" {
		acc.RateLimitFailures = 0
	}
	if status == "401" {
		acc.AuthStatus = store.AccountAuthStatusReauthRequired
	}
	if status == "429" {
		acc.RateLimitFailures++
		cooldown := accountpolicy.RateLimitCooldown(acc.RateLimitFailures)
		if acc.QuotaResetAt.IsZero() || acc.QuotaResetAt.Before(now.Add(cooldown)) {
			acc.QuotaResetAt = now.Add(cooldown)
		}
	}

	// Ensure the cache is updated as well
	for _, cached := range lb.cachedAccounts {
		if cached.ID == acc.ID {
			cached.StatusCode = status
			cached.LastAttempt = now
			cached.AuthStatus = acc.AuthStatus
			cached.RateLimitFailures = acc.RateLimitFailures
			cached.QuotaResetAt = acc.QuotaResetAt
			break
		}
	}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, "账号状态标记: "+status)
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
