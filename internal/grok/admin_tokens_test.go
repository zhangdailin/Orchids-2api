package grok

import (
	"testing"

	"orchids-api/internal/store"
)

func TestCollectAdminTokenEntries(t *testing.T) {
	payload := map[string]interface{}{
		"ssoBasic": []interface{}{
			"t0",
			map[string]interface{}{
				"token":     "sso=t1",
				"status":    "cooling",
				"quota":     float64(80),
				"use_count": float64(2),
				"note":      "note-1",
			},
		},
		"ssoSuper": []interface{}{
			map[string]interface{}{
				"token":  "t2",
				"status": "invalid",
			},
		},
		"ssoHeavy": []interface{}{
			map[string]interface{}{
				"token": "t3",
			},
		},
	}
	entries := collectAdminTokenEntries(payload)
	if len(entries) != 4 {
		t.Fatalf("entries len=%d want=4", len(entries))
	}
	if entries[0].Token != "t0" || entries[1].Token != "t1" || entries[2].Token != "t2" || entries[3].Token != "t3" {
		t.Fatalf("unexpected tokens: %+v", entries)
	}
	if entries[1].Status != "cooling" || entries[1].Quota != 80 || entries[1].UseCount != 2 || entries[1].Note != "note-1" {
		t.Fatalf("unexpected entry[1]: %+v", entries[1])
	}
	if entries[2].Status != "expired" {
		t.Fatalf("invalid status should normalize to expired, got=%q", entries[2].Status)
	}
}

func TestNormalizeAdminTokenStatus(t *testing.T) {
	if got := normalizeAdminTokenStatus("active"); got != "active" {
		t.Fatalf("status active=%q", got)
	}
	if got := normalizeAdminTokenStatus("cooling"); got != "cooling" {
		t.Fatalf("status cooling=%q", got)
	}
	if got := normalizeAdminTokenStatus("invalid"); got != "expired" {
		t.Fatalf("status invalid=%q want expired", got)
	}
	if got := normalizeAdminTokenStatus("anything"); got != "active" {
		t.Fatalf("unknown status=%q want active", got)
	}
}

func TestApplyTokenEntryToAccount_QuotaIsRemaining(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-basic", Pool: "ssoBasic", Quota: 35}
	applyTokenEntryToAccount(acc, entry)
	if acc.UsageLimit != 35 {
		t.Fatalf("usage_limit=%v want=35", acc.UsageLimit)
	}
	if acc.UsageCurrent != 35 {
		t.Fatalf("usage_current=%v want=35", acc.UsageCurrent)
	}
}

func TestApplyTokenEntryToAccount_LitePool(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-lite", Pool: "ssoLite", Quota: 60}
	applyTokenEntryToAccount(acc, entry)
	if acc.Subscription != "lite" {
		t.Fatalf("subscription=%q want lite", acc.Subscription)
	}
	if acc.UsageLimit != 70 {
		t.Fatalf("usage_limit=%v want=70", acc.UsageLimit)
	}
	if got := inferTokenPool(acc); got != "ssoLite" {
		t.Fatalf("inferTokenPool=%q want ssoLite", got)
	}
	if got := grokAccountPool(acc); got != "lite" {
		t.Fatalf("grokAccountPool=%q want lite", got)
	}
}

func TestApplyTokenEntryToAccount_SuperPoolUses140DefaultQuota(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-super", Pool: "ssoSuper", Quota: 120}
	applyTokenEntryToAccount(acc, entry)
	if acc.UsageLimit != 140 {
		t.Fatalf("usage_limit=%v want=140", acc.UsageLimit)
	}
	if acc.UsageCurrent != 120 {
		t.Fatalf("usage_current=%v want=120", acc.UsageCurrent)
	}
}

func TestApplyTokenEntryToAccount_HeavyPool(t *testing.T) {
	acc := &store.Account{}
	entry := adminTokenEntry{Token: "t-heavy", Pool: "ssoHeavy", Quota: 100}
	applyTokenEntryToAccount(acc, entry)
	if acc.Subscription != "heavy" {
		t.Fatalf("subscription=%q want heavy", acc.Subscription)
	}
	if got := inferTokenPool(acc); got != "ssoHeavy" {
		t.Fatalf("inferTokenPool=%q want ssoHeavy", got)
	}
	if got := grokAccountPool(acc); got != "heavy" {
		t.Fatalf("grokAccountPool=%q want heavy", got)
	}
}
