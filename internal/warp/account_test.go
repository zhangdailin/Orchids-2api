package warp

import (
	"testing"

	"orchids-api/internal/store"
)

func TestRefreshToken_UsesOnlyExplicitRefreshToken(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		RefreshToken: " refresh-token ",
		Token:        "legacy-refresh-token",
		ClientCookie: "refresh_token=cookie-token",
	}

	if got := RefreshToken(acc); got != "refresh-token" {
		t.Fatalf("RefreshToken()=%q want refresh-token", got)
	}
}

func TestRefreshToken_DoesNotFallbackToLegacyFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		Token:        "legacy-refresh-token",
		ClientCookie: "refresh_token=actual-refresh-token",
	}

	if got := RefreshToken(acc); got != "" {
		t.Fatalf("RefreshToken()=%q want empty", got)
	}
}

func TestRefreshToken_OnlyTrimsExplicitValue(t *testing.T) {
	t.Parallel()

	acc := &store.Account{RefreshToken: " refresh_token=not-parsed "}
	if got := RefreshToken(acc); got != "refresh_token=not-parsed" {
		t.Fatalf("RefreshToken()=%q want literal value", got)
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
