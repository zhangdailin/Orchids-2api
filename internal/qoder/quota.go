package qoder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

// The chat path is not the only place an account's allowance is visible. The
// gateway answers two control-plane reads that the reference implementation never
// used but the official CLI calls on login and on status display:
//
//	GET /api/v2/user/plan    plan tier, paid flag, feature switches, start date
//	GET /api/v2/quota/usage  the credit window: total, used, remaining, reset
//
// They are what turns "the account failed" into "the account has no credits left,
// here is the upgrade link", which is the difference between an operator guessing
// at a credential problem and knowing it is a billing state.

// Quota is one account's credit state as the gateway reports it.
type Quota struct {
	// PlanTier is the plan label, for example "Free".
	PlanTier string
	// UserType is the account class, for example "personal_standard".
	UserType string
	// PaidPlan reports whether the account is on a paid tier.
	PaidPlan bool
	// Limit, Used and Remaining describe the credit window. The gateway reports
	// them in credits.
	Limit     float64
	Used      float64
	Remaining float64
	// Exhausted is the gateway's own verdict. It is authoritative over the
	// arithmetic above: an account whose numbers have not refreshed yet can still
	// be flagged exhausted.
	Exhausted bool
	// Unit is the upstream unit, normally "credits".
	Unit string
	// PerModelCap is the percentage cap one model may consume of the window, when
	// the plan reports one.
	PerModelCap float64
	// ResetAt is the next allowance window boundary; PeriodEnd is when the quota
	// itself lapses.
	ResetAt   time.Time
	PeriodEnd time.Time
	// UpgradeURL is the page the gateway points an exhausted account at.
	UpgradeURL string
	// SyncedAt marks when this snapshot was observed.
	SyncedAt time.Time
}

// quotaUsageResponse is GET /api/v2/quota/usage.
type quotaUsageResponse struct {
	UserID          string  `json:"userId"`
	UserType        string  `json:"userType"`
	UsageType       string  `json:"usageType"`
	TotalUsagePct   float64 `json:"totalUsagePercentage"`
	IsQuotaExceeded bool    `json:"isQuotaExceeded"`
	ExpiresAt       int64   `json:"expiresAt"`
	LimitExceeded   bool    `json:"limitExceeded"`
	UpgradeURL      string  `json:"upgradeUrl"`
	ModelUsage      []struct {
		ModelKey         string  `json:"modelKey"`
		UsagePercentage  float64 `json:"usagePercentage"`
		TotalUsagePct    float64 `json:"totalUsagePercentage"`
		PerModelQuotaPct float64 `json:"perModelQuotaPercentage"`
	} `json:"modelUsage"`
	PromptCachePromptHitPct float64 `json:"promptCachePromptHitPercentage"`
	UserQuota               struct {
		Total      float64 `json:"total"`
		Used       float64 `json:"used"`
		Remaining  float64 `json:"remaining"`
		Percentage float64 `json:"percentage"`
		Unit       string  `json:"unit"`
	} `json:"userQuota"`
	OuterProviders      []json.RawMessage `json:"outerProviders"`
	LastRecoveryAt      int64             `json:"lastRecoveryAt"`
	IsPlanQuotaProrated bool              `json:"isPlanQuotaProrated"`
}

// planResponse is GET /api/v2/user/plan.
type planResponse struct {
	UserType       string `json:"user_type"`
	PlanTierName   string `json:"plan_tier_name"`
	IsPersonal     bool   `json:"is_personal_version"`
	IsPaidPlan     bool   `json:"is_paid_plan"`
	IsHighestTier  bool   `json:"is_highest_tier"`
	StartDate      int64  `json:"start_date"`
	EndDate        int64  `json:"end_date"`
	FeatureAllowed struct {
		Wiki           bool `json:"wiki"`
		Quest          bool `json:"quest"`
		CodeReview     bool `json:"code_review"`
		CommitIndexing bool `json:"commit_indexing"`
	} `json:"feature_allowed"`
}

// FetchQuota reads the account's credit state.
//
// A missing quota is not an error the caller must fail on: the usage endpoint is
// a separate read, and a deployment that cannot reach it should still serve chat.
// The returned error is therefore informative rather than fatal.
func (c *Client) FetchQuota(ctx context.Context) (*Quota, error) {
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	quota := &Quota{SyncedAt: now}

	usage, usageErr := c.getUsage(ctx, creds)
	if usageErr == nil {
		quota.UserType = strings.TrimSpace(usage.UserType)
		quota.Exhausted = usage.IsQuotaExceeded || usage.LimitExceeded
		quota.UpgradeURL = strings.TrimSpace(usage.UpgradeURL)
		quota.Limit = usage.UserQuota.Total
		quota.Used = usage.UserQuota.Used
		quota.Remaining = usage.UserQuota.Remaining
		quota.Unit = strings.TrimSpace(usage.UserQuota.Unit)
		if quota.Unit == "" {
			quota.Unit = "credits"
		}
		if usage.ExpiresAt > 0 {
			// normalizeMillis already yields seconds; unixSeconds must not
			// normalize a second time, or a far-future expiry wraps into 1978.
			quota.PeriodEnd = time.Unix(normalizeMillis(usage.ExpiresAt), 0)
		}
		quota.PerModelCap = highestModelQuotaShare(usage)
		// A window whose remaining share is zero is exhausted regardless of what
		// the flag says; the flag is stale between refreshes.
		if quota.Limit > 0 && quota.Remaining <= 0 {
			quota.Exhausted = true
		}
	}

	// The plan read supplies the tier label and the reset boundary. It is a
	// second call, so a failure leaves those fields empty rather than failing the
	// whole snapshot.
	if plan, planErr := c.getPlan(ctx, creds); planErr == nil {
		quota.PlanTier = strings.TrimSpace(plan.PlanTierName)
		quota.PaidPlan = plan.IsPaidPlan
		if quota.UserType == "" {
			quota.UserType = strings.TrimSpace(plan.UserType)
		}
		if quota.ResetAt.IsZero() && plan.StartDate > 0 {
			// The window is a rolling period: the CLI displays the next reset as
			// a full day boundary from the plan's start.
			quota.ResetAt = nextDailyBoundary(time.Unix(normalizeMillis(plan.StartDate), 0), now)
		}
	} else if status, statusErr := c.getStatus(ctx, creds); statusErr == nil {
		quota.PlanTier = strings.TrimSpace(status.UserTag)
		if status.NextResetAt > 0 {
			quota.ResetAt = time.Unix(normalizeMillis(status.NextResetAt), 0)
		}
	}

	if usageErr != nil && quota.PlanTier == "" {
		// Neither read worked: report the first cause rather than an empty
		// snapshot that looks like "no quota".
		return nil, usageErr
	}
	return quota, nil
}

// statusResponse is GET /api/v3/user/status.
type statusResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	UserType      string `json:"userType"`
	UserTag       string `json:"userTag"`
	Plan          string `json:"plan"`
	Quota         int64  `json:"quota"`
	IsQuotaExceed bool   `json:"isQuotaExceeded"`
	NextResetAt   int64  `json:"nextResetAt"`
	WhitelistStat string `json:"whitelistStatus"`
}

func (c *Client) getUsage(ctx context.Context, creds Credentials) (*quotaUsageResponse, error) {
	var out quotaUsageResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v2/quota/usage", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getPlan(ctx context.Context, creds Credentials) (*planResponse, error) {
	var out planResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v2/user/plan", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getStatus(ctx context.Context, creds Credentials) (*statusResponse, error) {
	var out statusResponse
	if err := c.getJSON(ctx, c.endpoints.openAPI+"/api/v3/user/status", creds, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getJSON performs one authenticated OpenAPI read. These endpoints take the
// device access token as a plain Bearer; they are not COSY-signed, which is why
// they are useful for diagnosing an account whose signed chat request failed.
func (c *Client) getJSON(ctx context.Context, url string, creds Credentials, out interface{}) error {
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.AccessToken))
	req.Header.Set("User-Agent", userAgent(c.clientVersion))
	if machine := strings.TrimSpace(c.machineID); machine != "" {
		// The CLI sends the device token alongside the bearer on this surface.
		req.Header.Set("Cosy-MachineToken", machine)
	}

	resp, err := c.control.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return apiError(http.MethodGet, url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", signPath(url), err)
	}
	return nil
}

// normalizeMillis folds a millisecond timestamp to seconds. The OpenAPI surface
// answers milliseconds while the COSY surface answers seconds.
func normalizeMillis(value int64) int64 {
	if value > 1e11 {
		return value / 1000
	}
	return value
}

// highestModelQuotaShare reports the largest per-model cap the plan declares, as
// a fraction. The gateway states it as a percentage.
func highestModelQuotaShare(usage *quotaUsageResponse) float64 {
	highest := 0.0
	for _, model := range usage.ModelUsage {
		for _, candidate := range []float64{model.PerModelQuotaPct, model.UsagePercentage, model.TotalUsagePct} {
			if candidate > highest && candidate <= 1 {
				highest = candidate
			}
		}
	}
	return highest
}

// nextDailyBoundary returns the next occurrence of the plan window's daily reset
// after now. The gateway reports the window start, not the next boundary, so the
// boundary is derived from the same time of day.
func nextDailyBoundary(start, now time.Time) time.Time {
	if start.IsZero() {
		return time.Time{}
	}
	if start.After(now) {
		return start
	}
	elapsed := now.Sub(start)
	days := elapsed / (24 * time.Hour)
	next := start.Add(time.Duration(days+1) * 24 * time.Hour)
	return next
}

// ApplyQuota folds a quota snapshot onto the account's scheduling fields.
//
// UsageCurrent carries what is LEFT, which is the convention the other channels
// use for a cycle allowance and what the console reads as "remaining".
func ApplyQuota(acc *store.Account, quota *Quota) {
	if acc == nil || quota == nil {
		return
	}
	previous := acc.QoderQuota
	acc.UsageLimit = quota.Limit
	acc.UsageCurrent = quota.Remaining
	acc.QoderQuota = store.QoderQuotaSnapshot{
		Limit:          quota.Limit,
		Remaining:      quota.Remaining,
		Used:           quota.Used,
		Exhausted:      quota.Exhausted,
		PlanTier:       quota.PlanTier,
		UserType:       quota.UserType,
		PaidPlan:       quota.PaidPlan,
		Unit:           quota.Unit,
		UpgradeURL:     quota.UpgradeURL,
		ResetAt:        quota.ResetAt,
		PeriodEnd:      quota.PeriodEnd,
		LastKnownLimit: firstPositive(quota.Limit, previous.Limit),
		SyncedAt:       quota.SyncedAt,
	}
	if !quota.ResetAt.IsZero() {
		acc.QuotaResetAt = quota.ResetAt
	}
}

// Exhausted reports whether a stored snapshot says the allowance is spent.
func (q *Quota) ExhaustedNow() bool {
	return q != nil && q.Exhausted
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
