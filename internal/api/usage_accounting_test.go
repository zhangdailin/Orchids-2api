package api

import (
	"testing"

	"orchids-api/internal/store"
)

func TestBuildQuotaMonthlyHasPresentationPriorityOverPercent(t *testing.T) {
	acc := buildAccount("supergrok", store.GrokBillingSnapshot{
		Weekly:  store.GrokQuotaWindow{HasUsage: true, UsagePercent: 40},
		Monthly: store.GrokQuotaWindow{HasLimit: true, Limit: 200, HasRemaining: true, Remaining: 150},
	})
	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_mode"); got != "monthly" {
		t.Fatalf("quota_mode=%q want monthly", got)
	}
	if got := fieldFloat(t, fields, "quota_limit"); got != 200 {
		t.Fatalf("quota_limit=%v want 200", got)
	}
	if got := fieldFloat(t, fields, "quota_weekly_usage_percent"); got != 40 {
		t.Fatalf("weekly detail=%v want 40", got)
	}
}
