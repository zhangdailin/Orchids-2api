package grok

import (
	"strings"

	"orchids-api/internal/store"
)

const (
	basicDefaultQuota float64 = 30
	liteDefaultQuota  float64 = 70
	superDefaultQuota float64 = 140
	heavyDefaultQuota float64 = 400
)

func InferQuotaLimit(acc *store.Account) float64 {
	if acc == nil {
		return basicDefaultQuota
	}
	if acc.UsageLimit > 0 {
		return acc.UsageLimit
	}
	sub := strings.ToLower(strings.TrimSpace(acc.Subscription))
	if strings.Contains(sub, "heavy") {
		return heavyDefaultQuota
	}
	if strings.Contains(sub, "super") || strings.Contains(sub, "pro") {
		return superDefaultQuota
	}
	if strings.Contains(sub, "lite") {
		return liteDefaultQuota
	}
	return basicDefaultQuota
}

func InferSubscriptionFromRateLimitInfo(info *RateLimitInfo) string {
	if info == nil || !info.HasLimit {
		return ""
	}
	switch limit := info.Limit; {
	case limit >= 150 || limit == 400:
		return "heavy"
	case limit == 50 || limit == 140:
		return "super"
	case limit == 25 || limit == 70 || limit == 12:
		return "lite"
	case limit == 30 || limit == 20 || limit == 8 || limit == 7:
		return "basic"
	default:
		return ""
	}
}

func ApplyQuotaInfo(acc *store.Account, info *RateLimitInfo) bool {
	if acc == nil || info == nil {
		return false
	}

	changed := false
	if sub := InferSubscriptionFromRateLimitInfo(info); sub != "" && acc.Subscription != sub {
		acc.Subscription = sub
		changed = true
	}
	if info.HasRemaining {
		limit := InferQuotaLimit(acc)
		if info.HasLimit && info.Limit > 0 {
			limit = float64(info.Limit)
		}
		remaining := float64(info.Remaining)
		if remaining < 0 {
			remaining = 0
		}
		if limit <= 0 {
			limit = basicDefaultQuota
		}
		if remaining > limit {
			limit = remaining
		}
		if acc.UsageLimit != limit {
			acc.UsageLimit = limit
			changed = true
		}
		if acc.UsageCurrent != remaining {
			acc.UsageCurrent = remaining
			changed = true
		}
	} else if info.HasLimit && info.Limit > 0 && acc.UsageLimit <= 0 {
		acc.UsageLimit = float64(info.Limit)
		changed = true
	}

	if !info.ResetAt.IsZero() && !acc.QuotaResetAt.Equal(info.ResetAt) {
		acc.QuotaResetAt = info.ResetAt
		changed = true
	}
	return changed
}
