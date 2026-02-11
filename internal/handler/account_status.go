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
	case hasExplicitHTTPStatus(lower, "401") || strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out") || strings.Contains(lower, "unauthorized"):
		return "401"
	case hasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case hasExplicitHTTPStatus(lower, "404"):
		return "404"
	case hasExplicitHTTPStatus(lower, "429") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits"):
		return "429"
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

	if err := store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("账号状态更新失败", "account_id", acc.ID, "status", status, "error", err)
		return
	}
	slog.Info("账号状态已标记", "account_id", acc.ID, "status", status)
}
