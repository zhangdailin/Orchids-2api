package handler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/store"
)

// accountStatusMu 保护并发的 markAccountStatus 调用，
// 避免多个 goroutine 同时修改同一 Account 的 StatusCode/LastAttempt。
var accountStatusMu sync.Mutex

func isWarpQuotaExhaustedError(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "no ai credits remaining") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "quota_limit") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits")
}

func isWarpCloudAgentForbiddenError(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "not allowed to use the provided cloud agent")
}

func markWarpQuotaExhausted(ctx context.Context, accountStore *store.Store, acc *store.Account) {
	if acc == nil || accountStore == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
		return
	}

	accountStatusMu.Lock()
	defer accountStatusMu.Unlock()

	// Do not use 429 here: an exhausted Warp account can still serve the free
	// auto-open/warp-chat path. A dedicated persisted status lets the load
	// balancer distinguish that state from a temporary rate limit after restart.
	acc.StatusCode = store.AccountStatusWarpQuotaExhausted
	acc.LastAttempt = time.Now()
	if acc.WarpMonthlyLimit <= 0 && acc.UsageLimit > 0 {
		acc.WarpMonthlyLimit = acc.UsageLimit
	}
	if acc.WarpMonthlyLimit > 0 {
		acc.WarpMonthlyRemaining = 0
		acc.WarpBonusRemaining = 0
		acc.UsageCurrent = acc.WarpMonthlyLimit
		if acc.UsageLimit <= 0 {
			acc.UsageLimit = acc.WarpMonthlyLimit
		}
	}

	if err := accountStore.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("Warp 额度状态更新失败", "account_id", acc.ID, "error", err)
		return
	}
	slog.Debug("Warp 额度已标记为不足", "account_id", acc.ID)
}
