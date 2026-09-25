package api

// Allowance projection: how one account's quota is rendered for the management
// API, one function per channel.
//
// Channels disagree about what their allowance even is — an upstream credit
// window, a rolling token estimate, a monthly request budget split into base and
// bonus, or nothing but a passive rate-limit header — so each channel keeps its
// own projection. What they share is the shape: fill the same fields and record
// where the numbers came from with applyQuotaProvenance. This used to be a single
// switch over the account type inside buildQuotaResponseFieldsWithUsage; the
// dispatch is now a lookup and each channel's rules are a named function.

import (
	"strings"
	"time"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// quotaProjector renders one channel's allowance into fields.
//
// limit and current are the account's generic usage slots, already clamped to
// non-negative. Most channels refine them from their own snapshot; observedTokens
// and usageObserved carry the usage this gateway measured itself, which only the
// Grok Free estimate uses.
type quotaProjector func(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool)

// quotaProjectors dispatches the allowance projection by account type. A channel
// that is absent falls back to the legacy rate-limit projection.
var quotaProjectors = map[string]quotaProjector{
	"qoder":     projectQoderQuota,
	"workbuddy": projectWorkBuddyQuota,
	"grok":      projectGrokQuota,
	"warp":      projectWarpQuota,
	"cline":     projectClineQuota,
}

// Cline publishes no numeric allowance: the free feed is a list, the inference
// cap is a rate limit expressed in prose, and there is no credit meter to read.
//
// The projection therefore answers "unsupported" instead of inventing a balance.
// Fabricating a limit here is what makes an account look spent when it is not,
// and the generic usage counters are reserved for channels that actually
// bill them.
func projectClineQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	fields["quota_limit"] = 0.0
	fields["quota_used"] = 0.0
	fields["quota_remaining"] = 0.0
	fields["quota_mode"] = "unmetered"
	fields["quota_unit"] = "requests"
	fields["quota_supported"] = false
	applyQuotaProvenance(fields, "unknown", "unknown", "",
		"Cline 未下发数值额度；达到推理上限时按 429 冷却处理", false, false)
}

// projectQuotaFields renders acc's allowance into fields.
func projectQuotaFields(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	projector := quotaProjectors[strings.ToLower(strings.TrimSpace(acc.AccountType))]
	if projector == nil {
		projector = projectLegacyQuota
	}
	projector(fields, acc, limit, current, observedTokens, usageObserved)
}

// The gateway reports the credit window directly, including its own
// exhausted verdict. The numbers are authoritative when a snapshot exists,
// and the verdict decides whether the account can spend at all — so the
// console can say "Free, 0 credits left, upgrade here" instead of showing
// a broken account.
func projectQoderQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	snapshot := acc.QoderQuota
	windowLimit := snapshot.Limit
	if windowLimit <= 0 {
		windowLimit = snapshot.LastKnownLimit
	}
	remaining := snapshot.Remaining
	if remaining < 0 {
		remaining = 0
	}
	used := snapshot.Used
	if used < 0 {
		used = 0
	}
	fields["quota_limit"] = windowLimit
	fields["quota_used"] = used
	fields["quota_remaining"] = remaining
	fields["quota_mode"] = "remaining"
	fields["quota_unit"] = util.FirstNonEmpty(snapshot.Unit, "credits")
	fields["quota_supported"] = !snapshot.SyncedAt.IsZero()
	fields["quota_plan"] = snapshot.PlanTier
	fields["quota_exhausted"] = snapshot.Exhausted
	fields["quota_upgrade_url"] = snapshot.UpgradeURL
	fields["quota_reset_at"] = snapshot.ResetAt
	// The gateway reports the window total itself, so a snapshot's windowLimit is
	// known even when it is zero (a Free plan with nothing left). Passing
	// limitKnown=false here would make consumers treat a reported window as an
	// estimate.
	if snapshot.Exhausted {
		// The allowance is spent: the credit window is the reason, and the
		// numbers are still reported rather than hidden.
		applyQuotaProvenance(fields, "upstreamQuota", "upstreamUsage", "", "", true, true)
		return
	}
	if snapshot.SyncedAt.IsZero() {
		applyQuotaProvenance(fields, "unknown", "upstreamUsage", "",
			"Qoder 额度接口未返回数据", false, false)
		return
	}
	applyQuotaProvenance(fields, "upstreamQuota", "upstreamUsage", "", "", true, true)
	return
}

// The meter reports the remaining credits of the current cycle; the
// generic UsageCurrent slot stores that remaining value for this channel,
// so "used" must be derived rather than read from UsageCurrent.
func projectWorkBuddyQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	snapshot := acc.WorkBuddyQuota
	quotaLimit := limit
	if snapshot.Limit > 0 {
		quotaLimit = snapshot.Limit
	}
	quotaRemaining := current
	if !snapshot.SyncedAt.IsZero() {
		quotaRemaining = snapshot.Remaining
	}
	if quotaLimit <= 0 {
		fields["quota_limit"] = 0.0
		fields["quota_used"] = 0.0
		fields["quota_remaining"] = 0.0
		fields["quota_mode"] = "unknown"
		fields["quota_unit"] = "credits"
		fields["quota_supported"] = false
		fields["quota_plan"] = snapshot.PackageName
		applyQuotaProvenance(fields, "unknown", "upstreamBilling", "",
			"WorkBuddy 计量接口未返回额度", false, !snapshot.SyncedAt.IsZero())
		return
	}
	if quotaRemaining < 0 {
		quotaRemaining = 0
	}
	if quotaRemaining > quotaLimit {
		quotaRemaining = quotaLimit
	}
	used := snapshot.Used
	if snapshot.SyncedAt.IsZero() {
		used = quotaLimit - quotaRemaining
	}
	if used < 0 {
		used = 0
	}
	fields["quota_limit"] = quotaLimit
	fields["quota_used"] = used
	fields["quota_remaining"] = quotaRemaining
	fields["quota_mode"] = "remaining"
	fields["quota_unit"] = util.FirstNonEmpty(snapshot.Unit, "credits")
	fields["quota_supported"] = !snapshot.SyncedAt.IsZero()
	fields["quota_plan"] = snapshot.PackageName
	fields["quota_consumed_units"] = snapshot.LastConsumedUnits
	fields["quota_package_remaining"] = snapshot.PackageRemaining
	workBuddyConfidence := ""
	if !snapshot.SyncedAt.IsZero() {
		workBuddyConfidence = "confirmed"
	}
	applyQuotaProvenance(fields, "paid", "upstreamBilling", workBuddyConfidence,
		"WorkBuddy 计量包返回的周期额度", quotaLimit > 0, !snapshot.SyncedAt.IsZero())
	if !snapshot.ResyncAt().IsZero() {
		fields["quota_reset_at"] = snapshot.ResyncAt().UTC().Format(time.RFC3339)
	}
}
func projectGrokQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	buildGrokBuildQuotaFields(fields, acc, observedTokens, usageObserved)
}

func projectWarpQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	baseLimit := limit
	if acc.WarpMonthlyLimit > 0 {
		baseLimit = acc.WarpMonthlyLimit
	}
	used := current
	if used > baseLimit && baseLimit > 0 {
		used = baseLimit
	}
	baseRemaining := acc.WarpMonthlyRemaining
	if baseRemaining <= 0 && baseLimit > 0 {
		baseRemaining = baseLimit - current
	}
	if baseRemaining < 0 {
		baseRemaining = 0
	}
	bonusRemaining := acc.WarpBonusRemaining
	if bonusRemaining < 0 {
		bonusRemaining = 0
	}
	remaining := baseRemaining + bonusRemaining
	fields["quota_limit"] = baseLimit
	fields["quota_used"] = used
	fields["quota_remaining"] = remaining
	fields["quota_mode"] = "warp_split"
	fields["quota_unit"] = "requests"
	fields["quota_base_limit"] = baseLimit
	fields["quota_base_remaining"] = baseRemaining
	fields["quota_bonus_remaining"] = bonusRemaining
	applyQuotaProvenance(fields, "paid", "upstreamBilling", "confirmed",
		"Warp 官方接口返回的月度额度与赠送额度", baseLimit > 0, false)
}

func projectLegacyQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	fields["quota_limit"] = limit
	remaining := current
	if remaining > limit && limit > 0 {
		remaining = limit
	}
	used := limit - remaining
	if used < 0 {
		used = 0
	}
	fields["quota_used"] = used
	fields["quota_remaining"] = remaining
	// A legacy account's numbers come from passive upstream headers, which are a
	// short-lived throttle window rather than a subscription balance.
	quotaType := "unknown"
	if limit > 0 {
		quotaType = "paid"
	}
	applyQuotaProvenance(fields, quotaType, "upstreamRateLimit", "observed",
		"来自上游限流响应头，不是套餐余额", limit > 0, false)
}

// buildGrokBuildQuotaFields projects one Build account's allowance.
//
// Upstream billing wins whenever it exists. When it does not, the account is not left
// as a bare "未知": the projection says whether the plan is known to be paid, can be
// inferred as Free, or is genuinely unknown — and a Free inference gets the estimated
// window plus the usage this gateway observed inside it. The estimate is marked
// estimated / limitKnown=false, so the number is a sense of scale rather than an
// official balance, and rate-limit headers stay where they belong (throttling, never
// a subscription balance).
func buildGrokBuildQuotaFields(fields map[string]interface{}, acc *store.Account, observedTokens int64, usageObserved bool) {
	weekly := acc.GrokBilling.Weekly
	monthly := acc.GrokBilling.Monthly
	fields["quota_mode"] = "unknown"
	fields["quota_unit"] = "build_credits"
	fields["quota_supported"] = false
	if monthly.HasLimit {
		fields["quota_monthly_limit"] = monthly.Limit
		fields["quota_monthly_remaining"] = monthly.Remaining
		fields["quota_limit"] = monthly.Limit
		fields["quota_used"] = max(0, monthly.Limit-monthly.Remaining)
		fields["quota_remaining"] = max(0, monthly.Remaining)
		fields["quota_mode"] = "monthly"
		fields["quota_unit"] = "build_credits"
		fields["quota_supported"] = true
		applyQuotaProvenance(fields, "paid", "upstreamBilling", "confirmed",
			"上游 Build 账单返回的月度额度", true, false)
	}
	if weekly.HasUsage {
		fields["quota_weekly_usage_percent"] = weekly.UsagePercent
		fields["quota_weekly_reset_at"] = weekly.ResetAt
		if !monthly.HasLimit {
			fields["quota_limit"] = 100.0
			fields["quota_used"] = weekly.UsagePercent
			fields["quota_remaining"] = max(0, 100-weekly.UsagePercent)
			fields["quota_mode"] = "weekly_percent"
			fields["quota_unit"] = "percent"
			fields["quota_supported"] = true
			fields["quota_reset_at"] = weekly.ResetAt
			applyQuotaProvenance(fields, "paid", "upstreamBilling", "confirmed",
				"上游 Build 账单返回的周度窗口", true, false)
		}
	}
	// Passive response headers are a minute-scale throttle, not a subscription
	// balance, so they are published separately and never folded into quota_limit.
	if acc.GrokRateLimits.Requests.HasLimit || acc.GrokRateLimits.Requests.HasRemaining {
		fields["rate_limit_requests"] = acc.GrokRateLimits.Requests
	}
	if acc.GrokRateLimits.Tokens.HasLimit || acc.GrokRateLimits.Tokens.HasRemaining {
		fields["rate_limit_tokens"] = acc.GrokRateLimits.Tokens
	}
	if weekly.HasUsage || monthly.HasLimit {
		return
	}
	// A Free window the upstream itself reported outranks every inference: it carries
	// the account's real actual/limit pair instead of a scale reference. It only does so
	// while the window is still current — once the rolling window has passed, those
	// numbers describe a window that no longer exists, and the account falls back to the
	// estimate (still Free, because the refusal proved it) instead of showing a stale 0.
	confirmed := acc.GrokFreeQuota
	confirmedCurrent := !confirmed.ResetAt.IsZero() && time.Now().Before(confirmed.ResetAt)
	if confirmed.HasLimit && !confirmed.ConfirmedAt.IsZero() && confirmedCurrent {
		limit := confirmed.Limit
		used := confirmed.Used
		if used < 0 {
			used = 0
		}
		if used > limit {
			used = limit
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = max(0, limit-used)
		fields["quota_mode"] = "confirmed_free"
		fields["quota_unit"] = "tokens"
		fields["quota_supported"] = true
		fields["quota_window_hours"] = int(grok.FreeBuildUsageWindow / time.Hour)
		if !confirmed.ResetAt.IsZero() {
			fields["quota_reset_at"] = confirmed.ResetAt
		}
		applyQuotaProvenance(fields, "free", "upstreamExhaustion", "confirmed",
			"上游额度耗尽时返回的真实 Free 窗口（tokens actual/limit）", true, true)
		return
	}
	switch verdict := grok.InferFreeProfile(acc); {
	case verdict.Inferred:
		limit := float64(grok.EstimatedFreeBuildTokenLimit)
		used := float64(0)
		if usageObserved && observedTokens > 0 {
			used = float64(observedTokens)
		}
		if used > limit {
			used = limit
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = max(0, limit-used)
		fields["quota_mode"] = "estimated_free"
		fields["quota_unit"] = "tokens"
		fields["quota_supported"] = true
		fields["quota_window_hours"] = int(grok.FreeBuildUsageWindow / time.Hour)
		applyQuotaProvenance(fields, "free", verdict.Source, "estimated",
			"上游未下发数值额度；按 Free 画像估算，用量为本网关在窗口内观测到的 token", false, usageObserved)
	case grok.BuildPlanIsPaid(acc.Subscription):
		applyQuotaProvenance(fields, "paid", "planMetadata", "confirmed",
			"官方身份接口报告为付费套餐，但未下发数值额度窗口", false, false)
	default:
		// Never synced, or the upstream has not said anything yet. Saying anything
		// more here would be an invention.
		applyQuotaProvenance(fields, "unknown", "unknown", "",
			"尚未同步到上游套餐或额度信息；点刷新立即同步", false, false)
	}
}
