package warp

import (
	"strings"
	"time"

	"orchids-api/internal/store"
)

// ResolveRefreshToken extracts the Warp refresh token from the fields that may
// carry it in this project or older imports.
func ResolveRefreshToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}

	candidates := []string{
		acc.RefreshToken,
		acc.Token,
		acc.ClientCookie,
	}

	for _, raw := range candidates {
		raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "Bearer "))
		if raw == "" {
			continue
		}
		normalized := normalizeRefreshToken(raw)
		if normalized == "" {
			continue
		}
		if looksLikeJWT(normalized) {
			continue
		}
		return normalized
	}

	return ""
}

func looksLikeJWT(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(strings.TrimSpace(p)) < 10 {
			return false
		}
	}
	return true
}

// InferSubscriptionFromRequestLimit maps Warp's official monthly credit quota
// to the public pricing tiers. Build and Business both expose 1,500 credits,
// so the quota alone cannot distinguish them.
func InferSubscriptionFromRequestLimit(info *RequestLimitInfo) string {
	if info == nil {
		return ""
	}
	if strings.TrimSpace(info.PlanTier) != "" {
		return strings.ToLower(strings.TrimSpace(info.PlanTier))
	}
	if strings.TrimSpace(info.PlanName) != "" {
		return strings.ToLower(strings.TrimSpace(info.PlanName))
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
		return "unknown"
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
	usedRequests := float64(info.RequestsUsedSinceLastRefresh)
	if usedRequests < 0 {
		usedRequests = 0
	}
	monthlyRemaining := monthlyLimit - usedRequests
	if monthlyRemaining < 0 {
		monthlyRemaining = 0
	}

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

func AccountQuotaExhausted(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
		return false
	}
	if acc.WarpMonthlyLimit > 0 {
		return acc.WarpMonthlyRemaining+acc.WarpBonusRemaining <= 0
	}
	if acc.UsageLimit > 0 {
		return acc.UsageLimit-acc.UsageCurrent <= 0
	}
	return false
}
