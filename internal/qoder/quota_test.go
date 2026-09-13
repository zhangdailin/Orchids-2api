package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func timeDate(year int, month time.Month, day, hour, min, sec int) time.Time {
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}

// TestFetchQuotaReadsTheWindowAndPlan proves the channel can describe an
// account's allowance. These two OpenAPI reads are what turn "the account
// failed" into "the account has no credits left", which is the difference
// between chasing a credential bug and acting on a billing state.
func TestFetchQuotaReadsTheWindowAndPlan(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-1" {
			t.Errorf("%s Authorization = %q", r.URL.Path, got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userId":"uid-1","userType":"personal_standard","usageType":"credits","isQuotaExceeded":true,"expiresAt":253402214400000,"upgradeUrl":"https://qoder.com/pricing?client=qoder","userQuota":{"total":0,"used":0,"remaining":0,"percentage":0,"unit":"credits"}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"user_type":"personal_standard","plan_tier_name":"Free","is_personal_version":true,"is_paid_plan":false,"start_date":1758508788374,"end_date":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	quota, err := client.FetchQuota(context.Background())
	if err != nil {
		t.Fatalf("FetchQuota() error = %v", err)
	}
	if quota.PlanTier != "Free" {
		t.Fatalf("PlanTier = %q, want Free", quota.PlanTier)
	}
	if quota.PaidPlan {
		t.Fatal("PaidPlan = true for a free account")
	}
	if !quota.Exhausted {
		t.Fatal("Exhausted = false, want the gateway's verdict")
	}
	if quota.UpgradeURL == "" {
		t.Fatal("UpgradeURL is empty, so the operator gets no next step")
	}
	if !quota.PeriodEnd.After(quota.SyncedAt) {
		t.Fatalf("PeriodEnd = %v, want a millisecond timestamp normalized to the future", quota.PeriodEnd)
	}
}

// TestApplyQuotaStoresRemainingAsUsageCurrent pins the scheduling convention: the
// shared UsageCurrent slot carries what is LEFT, matching every other channel.
func TestApplyQuotaStoresRemainingAsUsageCurrent(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "qoder"}
	ApplyQuota(acc, &Quota{
		Limit:      100,
		Used:       40,
		Remaining:  60,
		PlanTier:   "Pro",
		Unit:       "credits",
		UpgradeURL: "https://qoder.com/pricing?client=qoder",
	})
	if acc.UsageLimit != 100 || acc.UsageCurrent != 60 {
		t.Fatalf("usage = %v/%v, want limit 100 and remaining 60", acc.UsageLimit, acc.UsageCurrent)
	}
	if acc.QoderQuota.PlanTier != "Pro" || acc.QoderQuota.Used != 40 {
		t.Fatalf("snapshot = %+v", acc.QoderQuota)
	}
	if !acc.QoderQuota.SyncedAt.Equal(acc.QoderQuota.SyncedAt) {
		t.Fatal("snapshot carries no sync time")
	}
}

// TestQuotaExhaustedWithoutNumbersIsStillExhausted proves the gateway's verdict
// wins over arithmetic: a window whose counters have not refreshed yet but which
// the upstream already flagged is still spent.
func TestQuotaExhaustedWithoutNumbersIsStillExhausted(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/quota/usage" {
			_, _ = w.Write([]byte(`{"isQuotaExceeded":true,"userQuota":{"total":0,"used":0,"remaining":0,"unit":"credits"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewFromAccount(signedTestAccount(), nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)
	quota, err := client.FetchQuota(context.Background())
	if err != nil {
		t.Fatalf("FetchQuota() error = %v", err)
	}
	if !quota.Exhausted {
		t.Fatal("Exhausted = false despite the upstream verdict")
	}
}

// TestNextDailyBoundary proves the reset is derived, not invented.
func TestNextDailyBoundary(t *testing.T) {
	t.Parallel()

	start := timeDate(2026, 9, 21, 16, 0, 0)
	now := timeDate(2026, 9, 22, 3, 0, 0)
	next := nextDailyBoundary(start, now)
	if want := timeDate(2026, 9, 22, 16, 0, 0); !next.Equal(want) {
		t.Fatalf("nextDailyBoundary() = %v, want %v", next, want)
	}
	// A boundary already in the future is returned unchanged.
	future := timeDate(2026, 10, 1, 0, 0, 0)
	if got := nextDailyBoundary(future, now); !got.Equal(future) {
		t.Fatalf("nextDailyBoundary(future) = %v, want %v", got, future)
	}
}
