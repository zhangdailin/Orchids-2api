package workbuddy

import (
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestSummarizeQuota_AggregatesPackages(t *testing.T) {
	t.Parallel()

	// Mirrors a real personal account: a free-plan package plus a bonus pack,
	// both reported with fractional precise values.
	var payload resourceResponse
	payload.Response.Data.TotalCount = 2
	payload.Response.Data.Accounts = []meterAccount{
		{
			PackageName:           "Free Plan Subscription",
			CapacityUnit:          "credit",
			CapacitySize:          250,
			CapacityRemain:        47,
			CapacityRemainPrecise: "47.28",
			CycleCapacitySize:     250,
			CycleCapacityRemain:   47,
			CycleCapacitySizeP:    "250",
			CycleCapacityRemainP:  "47.28",
			CycleCapacityUsedP:    "202.72",
			CycleEndTime:          "2026-09-26 00:13:42",
		},
		{
			PackageName:           "Bonus Pack",
			CapacityUnit:          "credit",
			CapacitySize:          100,
			CapacityRemain:        100,
			CapacityRemainPrecise: "100",
			CycleCapacitySize:     100,
			CycleCapacityRemain:   100,
			CycleCapacitySizeP:    "100",
			CycleCapacityRemainP:  "100",
			CycleEndTime:          "2026-09-26 00:13:42",
		},
	}

	now := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	quota := summarizeQuota(payload, now)

	if quota.Limit != 350 {
		t.Fatalf("Limit = %v, want 350", quota.Limit)
	}
	if quota.Remaining != 147.28 {
		t.Fatalf("Remaining = %v, want 147.28 (precise values win)", quota.Remaining)
	}
	if used := quota.VisibleUsed(); used != 202.72 {
		t.Fatalf("VisibleUsed() = %v, want 202.72", used)
	}
	if quota.PackageRemaining != 147.28 {
		t.Fatalf("PackageRemaining = %v, want 147.28", quota.PackageRemaining)
	}
	if quota.PackageName == "" {
		t.Fatal("PackageName is empty")
	}
	wantReset := time.Date(2026, 9, 26, 0, 13, 42, 0, time.UTC)
	if !quota.ResetAt.Equal(wantReset) {
		t.Fatalf("ResetAt = %v, want %v", quota.ResetAt, wantReset)
	}
	if quota.SyncedAt.IsZero() {
		t.Fatal("SyncedAt is zero")
	}
}

func TestSummarizeQuota_EmptyMeterIsZeroNotError(t *testing.T) {
	t.Parallel()

	quota := summarizeQuota(resourceResponse{}, time.Now())
	if quota.Limit != 0 || quota.Remaining != 0 {
		t.Fatalf("quota = %+v, want a zero allowance", quota)
	}
	if used := quota.VisibleUsed(); used != 0 {
		t.Fatalf("VisibleUsed() = %v, want 0", used)
	}
}

func TestParseMeterTime_IgnoresPlaceholder(t *testing.T) {
	t.Parallel()

	if got := parseMeterTime("9999-99-99 99:99:99"); !got.IsZero() {
		t.Fatalf("placeholder parsed as %v, want zero", got)
	}
	if got := parseMeterTime(""); !got.IsZero() {
		t.Fatalf("empty parsed as %v, want zero", got)
	}
	if got := parseMeterTime("2026-09-26 00:13:42"); got.IsZero() {
		t.Fatal("valid timestamp did not parse")
	}
}

func TestApplyQuota_MapsRemainingIntoSchedulingFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy"}
	quota := &Quota{
		Limit:       350,
		Remaining:   147.28,
		ResetAt:     time.Date(2026, 9, 26, 0, 13, 42, 0, time.UTC),
		PackageName: "Free Plan Subscription",
		Unit:        "credit",
		SyncedAt:    time.Now(),
	}
	ApplyQuota(acc, quota)

	// UsageCurrent is the REMAINING value for this channel; the renderer derives
	// "used" from it, so storing used here would invert the quota bar.
	if acc.UsageLimit != 350 || acc.UsageCurrent != 147.28 {
		t.Fatalf("usage = %v/%v, want 147.28 remaining of 350", acc.UsageCurrent, acc.UsageLimit)
	}
	if acc.QuotaResetAt.IsZero() {
		t.Fatal("QuotaResetAt was not set from the cycle end")
	}
	if acc.WorkBuddyQuota.PackageName == "" || acc.WorkBuddyQuota.SyncedAt.IsZero() {
		t.Fatalf("snapshot = %+v", acc.WorkBuddyQuota)
	}
	if acc.WorkBuddyQuota.Used != 202.72 {
		t.Fatalf("snapshot used = %v, want 202.72", acc.WorkBuddyQuota.Used)
	}
}

func TestLastConsumedUnits_ResetsWithTheCycle(t *testing.T) {
	t.Parallel()

	cycleEnd := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	previous := store.WorkBuddyQuotaSnapshot{
		Limit:     350,
		Remaining: 200,
		Used:      150,
		ResetAt:   cycleEnd,
		SyncedAt:  time.Now().Add(-time.Hour),
	}

	grown := &Quota{Limit: 350, Remaining: 147.28, ResetAt: cycleEnd, SyncedAt: time.Now()}
	if got := grown.LastConsumedUnits(previous); got != 52 {
		// 202 whole credits consumed now vs 150 before: the counter reports the
		// whole-credit delta (fractional credit is not a countable "call").
		t.Fatalf("LastConsumedUnits() = %d, want 52", got)
	}

	// A new cycle re-arms the allowance; the counter must restart, not go negative.
	rearmed := &Quota{
		Limit:     350,
		Remaining: 350,
		ResetAt:   cycleEnd.Add(14 * 24 * time.Hour),
		SyncedAt:  time.Now(),
	}
	if got := rearmed.LastConsumedUnits(previous); got != 0 {
		t.Fatalf("LastConsumedUnits() after reset = %d, want 0", got)
	}

	// First ever snapshot reports the consumption observed so far.
	fresh := &Quota{Limit: 350, Remaining: 147.28, ResetAt: cycleEnd, SyncedAt: time.Now()}
	if got := fresh.LastConsumedUnits(store.WorkBuddyQuotaSnapshot{}); got != 202 {
		t.Fatalf("LastConsumedUnits() with no history = %d, want 202", got)
	}
}
