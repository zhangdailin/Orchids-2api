package warp

import (
	"strings"
	"time"

	"orchids-api/internal/store"
)

// RefreshToken returns the sole supported Warp credential: an explicitly
// stored refresh_token. Warp does not accept tokens from legacy account fields.
func RefreshToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(acc.RefreshToken, "\"'"))
}

// InferSubscriptionFromRequestLimit maps Warp's official monthly credit quota
// to the public pricing tiers. Build and Business both expose 1,500 credits,
// so the quota alone cannot distinguish them.
func InferSubscriptionFromRequestLimit(info *RequestLimitInfo) string {
	if info == nil {
		return ""
	}
	if info.IsUnlimited {
		return "enterprise"
	}

	limit := info.RequestLimit
	switch {
	case limit >= 18000:
		return "max"
	case limit >= 1500:
		return "build/business"
	case limit > 0:
		return "free"
	default:
		// limit == 0: either a free account or the GraphQL returned
		// empty data. Return "" so the existing subscription is preserved
		// rather than overwriting it with "unknown".
		return ""
	}
}

// ApplyRequestLimitInfoToAccount copies Warp's official request limit response
// into the account fields used by the admin UI and load balancer.
func ApplyRequestLimitInfoToAccount(acc *store.Account, info *RequestLimitInfo, bonuses []BonusGrant) {
	if acc == nil || info == nil {
		return
	}
	if tier := InferSubscriptionFromRequestLimit(info); tier != "" {
		acc.Subscription = tier
	}

	monthlyLimit := float64(info.RequestLimit)
	usedRequests := max(0, float64(info.RequestsUsedSinceLastRefresh))
	monthlyRemaining := max(0, monthlyLimit-usedRequests)

	bonusRemaining := 0.0
	for _, bg := range bonuses {
		if bg.RequestCreditsRemaining > 0 {
			bonusRemaining += float64(bg.RequestCreditsRemaining)
		}
	}

	acc.UsageLimit = monthlyLimit
	acc.UsageCurrent = usedRequests
	acc.WarpMonthlyLimit = monthlyLimit
	acc.WarpMonthlyRemaining = monthlyRemaining
	acc.WarpBonusRemaining = bonusRemaining
	if info.NextRefreshTime != "" {
		if t, err := time.Parse(time.RFC3339, info.NextRefreshTime); err == nil {
			acc.QuotaResetAt = t
		}
	}
}
