package api

import (
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

// buildAccount is a Grok Build (OAuth/Build provider) account with the given plan
// metadata and billing snapshot.
func buildAccount(plan string, billing store.GrokBillingSnapshot) *store.Account {
	return &store.Account{
		ID:           143,
		AccountType:  "grok",
		GrokProvider: grok.ProviderBuild,
		Subscription: plan,
		GrokBilling:  billing,
	}
}

func fieldString(t *testing.T, fields map[string]interface{}, key string) string {
	t.Helper()
	value, _ := fields[key].(string)
	return value
}

func fieldBool(t *testing.T, fields map[string]interface{}, key string) bool {
	t.Helper()
	value, _ := fields[key].(bool)
	return value
}

func fieldFloat(t *testing.T, fields map[string]interface{}, key string) float64 {
	t.Helper()
	value, _ := fields[key].(float64)
	return value
}

// TestBuildQuotaOfficialWindowIsConfirmed pins the first branch of the projection:
// upstream billing is authoritative and must keep its values and provenance.
func TestBuildQuotaOfficialWindowIsConfirmed(t *testing.T) {
	t.Parallel()

	acc := buildAccount("supergrok", store.GrokBillingSnapshot{
		Weekly:   store.GrokQuotaWindow{HasUsage: true, UsagePercent: 42, ResetAt: time.Now().Add(3 * 24 * time.Hour)},
		SyncedAt: time.Now(),
		Source:   "cli_billing",
	})

	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_mode"); got != "weekly_percent" {
		t.Fatalf("quota_mode=%q want weekly_percent", got)
	}
	if got := fieldFloat(t, fields, "quota_used"); got != 42 {
		t.Fatalf("quota_used=%v want 42", got)
	}
	if got := fieldString(t, fields, "quota_type"); got != "paid" {
		t.Fatalf("quota_type=%q want paid", got)
	}
	if got := fieldString(t, fields, "quota_source"); got != "upstreamBilling" {
		t.Fatalf("quota_source=%q want upstreamBilling", got)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "confirmed" {
		t.Fatalf("quota_confidence=%q want confirmed", got)
	}
	if !fieldBool(t, fields, "quota_limit_known") {
		t.Fatal("quota_limit_known=false for a window upstream actually reported")
	}
}

// TestBuildQuotaInfersFreeAndEstimatesTheWindow pins the reported gap: an OAuth
// account whose upstream publishes neither a plan name nor a numeric window used to
// fall back to "未知", which is indistinguishable from "never synced".
func TestBuildQuotaInfersFreeAndEstimatesTheWindow(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now(), Source: "cli_billing"})

	fields := buildQuotaResponseFieldsWithUsage(acc, 12000, true)
	if got := fieldString(t, fields, "quota_type"); got != "free" {
		t.Fatalf("quota_type=%q want free", got)
	}
	if got := fieldString(t, fields, "quota_source"); got != grok.FreeProfileSourceBilling {
		t.Fatalf("quota_source=%q want %q", got, grok.FreeProfileSourceBilling)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "estimated" {
		t.Fatalf("quota_confidence=%q want estimated", got)
	}
	if fieldBool(t, fields, "quota_limit_known") {
		t.Fatal("quota_limit_known=true for an estimated window: the UI would show it as a balance")
	}
	if !fieldBool(t, fields, "quota_observed") {
		t.Fatal("quota_observed=false although this gateway measured the usage")
	}
	if got := fieldFloat(t, fields, "quota_limit"); got != float64(grok.EstimatedFreeBuildTokenLimit) {
		t.Fatalf("quota_limit=%v want %d", got, grok.EstimatedFreeBuildTokenLimit)
	}
	if got := fieldFloat(t, fields, "quota_used"); got != 12000 {
		t.Fatalf("quota_used=%v want 12000", got)
	}
	if got := fieldFloat(t, fields, "quota_remaining"); got != float64(grok.EstimatedFreeBuildTokenLimit)-12000 {
		t.Fatalf("quota_remaining=%v", got)
	}
	if got := fields["quota_window_hours"]; got != 24 {
		t.Fatalf("quota_window_hours=%v want 24", got)
	}
	if got := fieldString(t, fields, "quota_unit"); got != "tokens" {
		t.Fatalf("quota_unit=%q want tokens", got)
	}
	// An unmeasured window must not be presented as measured usage of zero.
	unmeasured := buildQuotaResponseFields(acc)
	if fieldBool(t, unmeasured, "quota_observed") {
		t.Fatal("quota_observed=true although nothing was measured")
	}
	if got := fieldFloat(t, unmeasured, "quota_used"); got != 0 {
		t.Fatalf("quota_used=%v want 0 when nothing was measured", got)
	}
}

// TestBuildQuotaFreeFromOfficialPlanName covers the stronger Free signal: the
// upstream identity endpoint named the Free plan.
func TestBuildQuotaFreeFromOfficialPlanName(t *testing.T) {
	t.Parallel()

	acc := buildAccount("free", store.GrokBillingSnapshot{})
	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_type"); got != "free" {
		t.Fatalf("quota_type=%q want free", got)
	}
	if got := fieldString(t, fields, "quota_source"); got != grok.FreeProfileSourcePlan {
		t.Fatalf("quota_source=%q want %q", got, grok.FreeProfileSourcePlan)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "estimated" {
		t.Fatalf("quota_confidence=%q want estimated", got)
	}
}

// TestBuildQuotaPaidPlanWithoutWindowInventsNothing pins the boundary the estimate
// must never cross: a paid plan whose numeric window upstream does not publish keeps
// an unknown limit and no fabricated allowance.
func TestBuildQuotaPaidPlanWithoutWindowInventsNothing(t *testing.T) {
	t.Parallel()

	for _, plan := range []string{"supergrok", "x_premium", "supergrok_heavy", "supergrok_lite"} {
		acc := buildAccount(plan, store.GrokBillingSnapshot{SyncedAt: time.Now()})
		fields := buildQuotaResponseFields(acc)
		if got := fieldString(t, fields, "quota_type"); got != "paid" {
			t.Fatalf("%s: quota_type=%q want paid", plan, got)
		}
		if got := fieldString(t, fields, "quota_source"); got != "planMetadata" {
			t.Fatalf("%s: quota_source=%q want planMetadata", plan, got)
		}
		if fieldBool(t, fields, "quota_limit_known") {
			t.Fatal("a window upstream never reported cannot be a known limit")
		}
		if got := fieldFloat(t, fields, "quota_limit"); got != 0 {
			t.Fatalf("%s: quota_limit=%v want 0 (no invented allowance)", plan, got)
		}
		if fieldBool(t, fields, "quota_supported") {
			t.Fatalf("%s: quota_supported=true for an unknown window", plan)
		}
	}
}

// TestBuildQuotaUnsyncedStaysUnknown pins that "never synced" is not turned into Free:
// an inference needs a signal, not the absence of one.
func TestBuildQuotaUnsyncedStaysUnknown(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{})
	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_type"); got != "unknown" {
		t.Fatalf("quota_type=%q want unknown", got)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "" {
		t.Fatalf("quota_confidence=%q want empty for an account with no signal", got)
	}
	if fieldBool(t, fields, "quota_supported") {
		t.Fatal("quota_supported=true without any upstream data")
	}
	if got := fieldString(t, fields, "quota_note"); got == "" {
		t.Fatal("a bare unknown quota must still explain itself")
	}
}

// TestFreeProfileInferenceRequiresASignal is the unit-level pin for the rule above.
func TestFreeProfileInferenceRequiresASignal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		acc    *store.Account
		want   bool
		source string
	}{
		{"synced zero billing", buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()}), true, grok.FreeProfileSourceBilling},
		{"official free plan", buildAccount("free", store.GrokBillingSnapshot{}), true, grok.FreeProfileSourcePlan},
		{"never synced", buildAccount("", store.GrokBillingSnapshot{}), false, ""},
		{"unknown plan", buildAccount("unknown", store.GrokBillingSnapshot{}), false, ""},
		{"paid plan", buildAccount("supergrok", store.GrokBillingSnapshot{SyncedAt: time.Now()}), false, ""},
		{"paid plan with a window", buildAccount("supergrok", store.GrokBillingSnapshot{SyncedAt: time.Now(), Weekly: store.GrokQuotaWindow{HasUsage: true}}), false, ""},
		{"web account", &store.Account{AccountType: "grok", GrokProvider: grok.ProviderWeb, Subscription: "free"}, false, ""},
	}
	for _, tc := range cases {
		verdict := grok.InferFreeProfile(tc.acc)
		if verdict.Inferred != tc.want {
			t.Fatalf("%s: inferred=%v want %v", tc.name, verdict.Inferred, tc.want)
		}
		if verdict.Source != tc.source {
			t.Fatalf("%s: source=%q want %q", tc.name, verdict.Source, tc.source)
		}
	}
}

// TestObservedTokensByAccountUsesOnlyTheFreeWindow pins the measurement behind the
// estimate: usage comes from this gateway's own journal, only inside the window, and a
// failed measurement is reported as "not measured" instead of zero.
func TestObservedTokensByAccountUsesOnlyTheFreeWindow(t *testing.T) {
	s, _ := newTestStore(t, "quota-observed:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	recent := requestEvent("req-recent", "success", 200)
	recent.AccountID = 143
	recent.InputTokens = 900
	recent.OutputTokens = 1100
	recent.Timestamp = time.Now().Add(-2 * time.Hour)

	older := requestEvent("req-older", "success", 200)
	older.AccountID = 143
	older.InputTokens = 40000
	older.OutputTokens = 40000
	older.Timestamp = time.Now().Add(-30 * time.Hour)

	other := requestEvent("req-other", "success", 200)
	other.AccountID = 144
	other.InputTokens = 5000
	other.Timestamp = time.Now().Add(-time.Hour)

	seedJournal(t, a, []audit.Event{older, recent, other})

	usage, ok := a.observedTokensByAccount(t.Context(), time.Now().Add(-grok.FreeBuildUsageWindow))
	if !ok {
		t.Fatal("the measurement reported failure on a healthy store")
	}
	if got := usage[143]; got != 2000 {
		t.Fatalf("account 143 usage = %d, want the 2000 tokens inside the window", got)
	}
	if got := usage[144]; got != 5000 {
		t.Fatalf("account 144 usage = %d, want 5000", got)
	}

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()})
	fields := buildQuotaResponseFieldsWithUsage(acc, usage[acc.ID], true)
	if got := fieldFloat(t, fields, "quota_used"); got != 2000 {
		t.Fatalf("quota_used=%v want the in-window measurement", got)
	}
	if !fieldBool(t, fields, "quota_observed") {
		t.Fatal("quota_observed=false although the measurement succeeded")
	}
}

// TestBuildQuotaConfirmedFreeWindowReplacesTheEstimate pins the upgrade path: once the
// upstream reports the real actual/limit pair, the estimate is replaced by a confirmed
// balance instead of lingering next to it.
func TestBuildQuotaConfirmedFreeWindowReplacesTheEstimate(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()})
	acc.GrokFreeQuota = store.GrokFreeQuotaSnapshot{
		Used: 300000, Limit: 300000, HasLimit: true,
		ConfirmedAt: time.Now(), ResetAt: time.Now().Add(24 * time.Hour),
	}

	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_mode"); got != "confirmed_free" {
		t.Fatalf("quota_mode=%q want confirmed_free", got)
	}
	if got := fieldString(t, fields, "quota_source"); got != "upstreamExhaustion" {
		t.Fatalf("quota_source=%q want upstreamExhaustion", got)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "confirmed" {
		t.Fatalf("quota_confidence=%q want confirmed", got)
	}
	if !fieldBool(t, fields, "quota_limit_known") {
		t.Fatal("a window the upstream reported has a known limit")
	}
	if got := fieldFloat(t, fields, "quota_limit"); got != 300000 {
		t.Fatalf("quota_limit=%v, want the reported limit rather than the estimate", got)
	}
	if got := fieldFloat(t, fields, "quota_used"); got != 300000 {
		t.Fatalf("quota_used=%v want 300000", got)
	}
	if got := fieldFloat(t, fields, "quota_remaining"); got != 0 {
		t.Fatalf("quota_remaining=%v want 0", got)
	}

	// Once the rolling window has passed, those numbers describe a window that no longer
	// exists: the account falls back to the estimate, but the refusal still proves Free.
	expired := buildAccount("", store.GrokBillingSnapshot{})
	expired.GrokFreeQuota = store.GrokFreeQuotaSnapshot{
		Used: 300000, Limit: 300000, HasLimit: true,
		ConfirmedAt: time.Now().Add(-48 * time.Hour), ResetAt: time.Now().Add(-24 * time.Hour),
	}
	staleFields := buildQuotaResponseFields(expired)
	if got := fieldString(t, staleFields, "quota_mode"); got != "estimated_free" {
		t.Fatalf("expired window: quota_mode=%q want estimated_free", got)
	}
	if got := fieldString(t, staleFields, "quota_source"); got != grok.FreeProfileSourceExhaustion {
		t.Fatalf("expired window: quota_source=%q want the Free inference kept", got)
	}
	if fieldBool(t, staleFields, "quota_limit_known") {
		t.Fatal("an expired window cannot still be a known current limit")
	}
	if got := fieldFloat(t, staleFields, "quota_limit"); got != float64(grok.EstimatedFreeBuildTokenLimit) {
		t.Fatalf("expired window: quota_limit=%v, want the estimate", got)
	}
}

// TestWebQuotaKeepsItsProvenance makes sure the new fields do not become Build-only:
// the UI labels every channel's number with where it came from.
func TestWebQuotaKeepsItsProvenance(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "grok",
		GrokProvider: grok.ProviderWeb,
		GrokWebQuota: store.GrokWebQuotaSnapshot{
			Auto:     store.GrokQuotaWindow{Limit: 30, Remaining: 12, HasLimit: true, HasRemaining: true},
			SyncedAt: time.Now(),
		},
	}

	fields := buildQuotaResponseFields(acc)
	if got := fieldString(t, fields, "quota_source"); got != "upstreamBilling" {
		t.Fatalf("quota_source=%q want upstreamBilling", got)
	}
	if got := fieldString(t, fields, "quota_confidence"); got != "confirmed" {
		t.Fatalf("quota_confidence=%q want confirmed", got)
	}
	if !fieldBool(t, fields, "quota_limit_known") {
		t.Fatal("a Web window upstream reported has a known limit")
	}
	if got := fieldFloat(t, fields, "quota_remaining"); got != 12 {
		t.Fatalf("quota_remaining=%v want 12", got)
	}
}
