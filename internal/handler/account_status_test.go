package handler

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
)

func TestClassifyAccountStatus(t *testing.T) {
	tests := []struct {
		name     string
		errStr   string
		expected string
	}{
		{
			name:     "Explicit 401",
			errStr:   "HTTP 401 Unauthorized",
			expected: "401",
		},
		{
			name:     "Explicit 403",
			errStr:   "HTTP 403 Forbidden",
			expected: "403",
		},
		{
			name:     "Model not found should not mark account",
			errStr:   "grok upstream status=403 body={\"error\":{\"code\":7,\"message\":\"Model is not found\",\"details\":[]}}",
			expected: "",
		},
		{
			name:     "Explicit 404",
			errStr:   "HTTP 404 Not Found",
			expected: "404",
		},
		{
			name:     "Signed out message",
			errStr:   "User is signed out",
			expected: "401",
		},
		{
			name:     "No active Clerk sessions",
			errStr:   "no active sessions found",
			expected: "401",
		},
		{
			name:     "Missing Orchids client cookie",
			errStr:   "signed out: missing orchids client cookie",
			expected: "401",
		},
		{
			name:     "Forbidden message",
			errStr:   "Access forbidden",
			expected: "403",
		},
		{
			name:     "Explicit 429",
			errStr:   "HTTP 429 Too Many Requests",
			expected: "429",
		},
		{
			name:     "Puter insufficient funds maps to cooldown",
			errStr:   "puter API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.",
			expected: "402",
		},
		{
			name:     "Quota exceeded message",
			errStr:   "No remaining quota: No AI requests remaining",
			expected: "429",
		},
		{
			name:     "Rate limit message",
			errStr:   "Rate limit exceeded",
			expected: "429",
		},
		{
			name:     "Credits exhausted message",
			errStr:   "You have run out of credits. Please upgrade your plan to continue.",
			expected: "402",
		},
		{
			name:     "Server error (ignored)",
			errStr:   "HTTP 500 Internal Server Error",
			expected: "",
		},
		{
			name:     "Unknown error (ignored)",
			errStr:   "Something went wrong",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAccountStatus(tt.errStr)
			if got != tt.expected {
				t.Errorf("classifyAccountStatus(%q) = %q, want %q", tt.errStr, got, tt.expected)
			}
		})
	}
}

func TestIsWarpQuotaExhaustedError(t *testing.T) {
	if !isWarpQuotaExhaustedError(`warp stream request failed: HTTP 429: {"error":"No AI credits remaining"}`) {
		t.Fatal("expected No AI credits remaining to be treated as Warp quota exhausted")
	}
	if isWarpQuotaExhaustedError("warp stream request failed: HTTP 429 Too Many Requests") {
		t.Fatal("expected generic 429 to remain rate limit, not quota exhausted")
	}
}

func TestMarkWarpQuotaExhausted(t *testing.T) {
	mini := miniredis.RunT(t)
	defer mini.Close()

	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "warp_quota_status_test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	acc := &store.Account{
		AccountType:          "warp",
		Enabled:              true,
		UsageLimit:           60,
		UsageCurrent:         2,
		WarpMonthlyLimit:     60,
		WarpMonthlyRemaining: 58,
		WarpBonusRemaining:   0,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	markWarpQuotaExhausted(ctx, s, acc)

	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.StatusCode != "429" {
		t.Fatalf("StatusCode=%q want 429", got.StatusCode)
	}
	if got.WarpMonthlyRemaining != 0 || got.WarpBonusRemaining != 0 {
		t.Fatalf("remaining=%v bonus=%v want 0,0", got.WarpMonthlyRemaining, got.WarpBonusRemaining)
	}
	if got.UsageCurrent != got.WarpMonthlyLimit {
		t.Fatalf("UsageCurrent=%v want WarpMonthlyLimit=%v", got.UsageCurrent, got.WarpMonthlyLimit)
	}
}
