package handler

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/store"
)

func classifyAccountStatus(errStr string) string {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "quota_exceeded") || strings.Contains(lower, "quota exceeded") || strings.Contains(lower, "quota"):
		return "quota_exceeded"
	case hasExplicitHTTPStatus(lower, "401") || strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out") || strings.Contains(lower, "unauthorized"):
		return "401"
	case hasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case hasExplicitHTTPStatus(lower, "404"):
		return "404"
	default:
		return ""
	}
}

func hasExplicitHTTPStatus(lower string, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || lower == "" {
		return false
	}
	patterns := []string{
		"http " + code,
		"http/1.1 " + code,
		"http/2 " + code,
		"status " + code,
		"status=" + code,
		"status:" + code,
		"statuscode " + code,
		"statuscode=" + code,
		"status code " + code,
		"code " + code,
		"code=" + code,
		"code:" + code,
		"response status " + code,
		"response code " + code,
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func markAccountStatus(ctx context.Context, store *store.Store, acc *store.Account, status string) {
	if acc == nil || store == nil || status == "" {
		return
	}

	now := time.Now()

	// 避免重复标记同一状态，防止冷却计时器被反复重置
	if acc.StatusCode == status {
		slog.Debug("账号状态已存在，跳过重复标记", "account_id", acc.ID, "status", status)
		return
	}

	acc.StatusCode = status
	acc.LastAttempt = now
	if status == "quota_exceeded" {
		acc.QuotaResetAt = nextMonthStart(now)
	}

	if err := store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("账号状态更新失败", "account_id", acc.ID, "status", status, "error", err)
		return
	}
	slog.Info("账号状态已标记", "account_id", acc.ID, "status", status)
}

// nextMonthStart 与 loadbalancer 包中的同名函数重复，
// 但因跨包无法直接共享，保留各自副本。
func nextMonthStart(now time.Time) time.Time {
	year, month, _ := now.Date()
	loc := now.Location()
	if month == time.December {
		return time.Date(year+1, time.January, 1, 0, 0, 0, 0, loc)
	}
	return time.Date(year, month+1, 1, 0, 0, 0, 0, loc)
}
