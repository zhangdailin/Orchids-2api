package handler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

// accountStatusMu 保护并发的 markAccountStatus 调用，
// 避免多个 goroutine 同时修改同一 Account 的 StatusCode/LastAttempt。
var accountStatusMu sync.Mutex

// classifyAccountStatus delegates to the centralized errors package.
func classifyAccountStatus(errStr string) string {
	return apperrors.ClassifyAccountStatus(errStr)
}

func isWarpQuotaExhaustedError(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "no ai credits remaining") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "quota_limit") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits")
}

func markAccountStatus(ctx context.Context, store *store.Store, acc *store.Account, status string) {
	if acc == nil || store == nil || status == "" {
		return
	}

	accountStatusMu.Lock()
	defer accountStatusMu.Unlock()

	now := time.Now()
	acc.StatusCode = status
	acc.LastAttempt = now

	if err := store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("账号状态更新失败", "account_id", acc.ID, "status", status, "error", err)
		return
	}
	slog.Debug("账号状态已标记", "account_id", acc.ID, "status", status)
}

func markWarpQuotaExhausted(ctx context.Context, store *store.Store, acc *store.Account) {
	if acc == nil || store == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
		return
	}

	accountStatusMu.Lock()
	defer accountStatusMu.Unlock()

	acc.StatusCode = "429"
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

	if err := store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("Warp 额度状态更新失败", "account_id", acc.ID, "error", err)
		return
	}
	slog.Debug("Warp 额度已标记为不足", "account_id", acc.ID)
}
