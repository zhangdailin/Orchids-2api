package warp

import (
	"testing"

	"orchids-api/internal/store"
)

func TestResolveRefreshToken_UsesLegacyTokenField(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType: "warp",
		Token:       "legacy-refresh-token",
	}

	if got := ResolveRefreshToken(acc); got != "legacy-refresh-token" {
		t.Fatalf("ResolveRefreshToken()=%q want legacy-refresh-token", got)
	}
}

func TestResolveRefreshToken_PrefersNonJWTOverRuntimeToken(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		Token:        "aaaaaaaaaa.bbbbbbbbbb.cccccccccc",
		ClientCookie: "refresh_token=actual-refresh-token",
	}

	if got := ResolveRefreshToken(acc); got != "actual-refresh-token" {
		t.Fatalf("ResolveRefreshToken()=%q want actual-refresh-token", got)
	}
}

func TestInferSubscriptionFromRequestLimit_MapsWarpPricingTiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info *RequestLimitInfo
		want string
	}{
		{
			name: "free",
			info: &RequestLimitInfo{RequestLimit: 60},
			want: "free",
		},
		{
			name: "build business",
			info: &RequestLimitInfo{RequestLimit: 1500},
			want: "build/business",
		},
		{
			name: "max",
			info: &RequestLimitInfo{RequestLimit: 18000},
			want: "max",
		},
		{
			name: "enterprise",
			info: &RequestLimitInfo{IsUnlimited: true},
			want: "enterprise",
		},
		{
			name: "official tier wins",
			info: &RequestLimitInfo{PlanTier: "Build", RequestLimit: 60},
			want: "build",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := InferSubscriptionFromRequestLimit(tt.info); got != tt.want {
				t.Fatalf("InferSubscriptionFromRequestLimit()=%q want %q", got, tt.want)
			}
		})
	}
}

func TestApplyRequestLimitInfoToAccount_OverwritesStaleWarpTier(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "warp", Subscription: "free"}
	info := &RequestLimitInfo{
		RequestLimit:                 1500,
		RequestsUsedSinceLastRefresh: 423,
		NextRefreshTime:              "2026-06-14T02:24:43Z",
	}

	ApplyRequestLimitInfoToAccount(acc, info, nil)

	if acc.Subscription != "build/business" {
		t.Fatalf("Subscription=%q want build/business", acc.Subscription)
	}
	if acc.WarpMonthlyLimit != 1500 || acc.WarpMonthlyRemaining != 1077 {
		t.Fatalf("unexpected warp quota limit=%v remaining=%v", acc.WarpMonthlyLimit, acc.WarpMonthlyRemaining)
	}
	if acc.QuotaResetAt.IsZero() {
		t.Fatal("QuotaResetAt was not parsed")
	}
}

func TestAccountQuotaExhausted(t *testing.T) {
	tests := []struct {
		name string
		acc  *store.Account
		want bool
	}{
		{
			name: "paid account with no monthly or bonus remaining",
			acc: &store.Account{
				AccountType:          "warp",
				WarpMonthlyLimit:     1500,
				WarpMonthlyRemaining: 0,
				WarpBonusRemaining:   0,
				UsageLimit:           1500,
				UsageCurrent:         1500,
			},
			want: true,
		},
		{
			name: "bonus keeps paid pool alive",
			acc: &store.Account{
				AccountType:          "warp",
				WarpMonthlyLimit:     1500,
				WarpMonthlyRemaining: 0,
				WarpBonusRemaining:   1,
			},
			want: false,
		},
		{
			name: "warp quota beats stale generic usage",
			acc: &store.Account{
				AccountType:          "warp",
				WarpMonthlyLimit:     1500,
				WarpMonthlyRemaining: 0,
				WarpBonusRemaining:   0,
				UsageLimit:           1500,
				UsageCurrent:         100,
			},
			want: true,
		},
		{
			name: "generic usage fallback",
			acc: &store.Account{
				AccountType:  "warp",
				UsageLimit:   1500,
				UsageCurrent: 1500,
			},
			want: true,
		},
		{
			name: "unknown quota is not treated as exhausted",
			acc:  &store.Account{AccountType: "warp"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AccountQuotaExhausted(tt.acc); got != tt.want {
				t.Fatalf("AccountQuotaExhausted()=%v want %v", got, tt.want)
			}
		})
	}
}
