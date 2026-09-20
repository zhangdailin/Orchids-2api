package grok

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/store"
)

// buildAcc is a Build OAuth account, the only provider the Build Free window (and
// therefore this parser) applies to.
func buildAcc() *store.Account {
	return &store.Account{ID: 143, AccountType: "grok", GrokProvider: ProviderBuild}
}

func TestAccountUsableForModelHonorsBuildFreeReset(t *testing.T) {
	acc := buildAcc()
	acc.GrokFreeQuota.ResetAt = time.Now().Add(time.Hour)
	if accountUsableForModel(context.Background(), acc) {
		t.Fatal("Build Free account was selected before its confirmed reset")
	}
	acc.GrokFreeQuota.ResetAt = time.Now().Add(-time.Second)
	if !accountUsableForModel(context.Background(), acc) {
		t.Fatal("Build Free account remained unavailable after its reset")
	}
}

// TestApplyFreeQuotaExhaustionReadsTheRealWindow pins the one place a Free allowance
// stops being an estimate: the refusal the upstream sends after the included free
// usage is spent carries the account's real actual/limit pair.
func TestApplyFreeQuotaExhaustionReadsTheRealWindow(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"You have used all the included free usage for model grok-4.6. Tokens (actual/limit): 500123/500000"}}`)

	if !ApplyFreeQuotaExhaustion(acc, body) {
		t.Fatal("a Free-usage-exhausted refusal was not recognised")
	}
	if !acc.GrokFreeQuota.HasLimit {
		t.Fatal("the actual/limit pair was not read out of the refusal")
	}
	if acc.GrokFreeQuota.Used != 500123 || acc.GrokFreeQuota.Limit != 500000 {
		t.Fatalf("used/limit = %v/%v, want 500123/500000", acc.GrokFreeQuota.Used, acc.GrokFreeQuota.Limit)
	}
	if acc.GrokFreeQuota.ConfirmedAt.IsZero() {
		t.Fatal("a confirmed window must record when it was confirmed")
	}
	if got := time.Until(acc.GrokFreeQuota.ResetAt); got <= 0 || got > FreeBuildUsageWindow+time.Minute {
		t.Fatalf("reset in %v, want within the rolling %v window", got, FreeBuildUsageWindow)
	}
	// The confirmed window is the strongest Free signal there is.
	verdict := InferFreeProfile(acc)
	if !verdict.Inferred || verdict.Source != FreeProfileSourceExhaustion {
		t.Fatalf("verdict = %+v, want an inference sourced from %q", verdict, FreeProfileSourceExhaustion)
	}
}

// TestApplyFreeQuotaExhaustionWithoutNumbers pins the degraded case: the refusal is
// still proof of a Free account even when the pair cannot be parsed, and it must not
// be recorded as a limit of zero.
func TestApplyFreeQuotaExhaustionWithoutNumbers(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted"}}`)
	if !ApplyFreeQuotaExhaustion(acc, body) {
		t.Fatal("a Free refusal without numbers was not recognised")
	}
	if acc.GrokFreeQuota.HasLimit {
		t.Fatal("HasLimit=true without a readable pair")
	}
	if acc.GrokFreeQuota.ConfirmedAt.IsZero() {
		t.Fatal("the confirmation timestamp is still useful knowledge")
	}
	if verdict := InferFreeProfile(acc); !verdict.Inferred || verdict.Source != FreeProfileSourceExhaustion {
		t.Fatalf("verdict = %+v, want a Free inference from the refusal", verdict)
	}
}

// TestApplyFreeQuotaExhaustionKeepsAPreviouslyReportedLimit pins that a later refusal
// which cannot be parsed does not downgrade the window back to "unknown number": the
// real actual/limit pair stays until the upstream reports a different one.
func TestApplyFreeQuotaExhaustionKeepsAPreviouslyReportedLimit(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	if !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"Tokens (actual/limit): 500123/500000"}}`)) {
		t.Fatal("the first refusal was not recognised")
	}
	confirmedAt := acc.GrokFreeQuota.ConfirmedAt

	if !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted"}}`)) {
		t.Fatal("the second refusal was not recognised")
	}
	if !acc.GrokFreeQuota.HasLimit || acc.GrokFreeQuota.Limit != 500000 || acc.GrokFreeQuota.Used != 500123 {
		t.Fatalf("the previously confirmed pair was lost: %+v", acc.GrokFreeQuota)
	}
	if !acc.GrokFreeQuota.ConfirmedAt.After(confirmedAt) {
		t.Fatal("the confirmation timestamp was not refreshed")
	}

	// A NEWER refusal with a different pair is the truth for the current window.
	if !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"Tokens (actual/limit): 12/900000"}}`)) {
		t.Fatal("the third refusal was not recognised")
	}
	if acc.GrokFreeQuota.Limit != 900000 || acc.GrokFreeQuota.Used != 12 {
		t.Fatalf("a newer reported pair did not replace the old one: %+v", acc.GrokFreeQuota)
	}
}

func TestApplyFreeQuotaExhaustionIgnoresEverythingElse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		acc  *store.Account
		body string
	}{
		{"ordinary 429", buildAcc(), `{"error":{"code":"rate_limit_exceeded"}}`},
		{"paid spending limit", buildAcc(), `{"error":{"code":"personal-team-blocked:spending-limit"}}`},
		{"web account", &store.Account{ID: 1, AccountType: "grok", GrokProvider: ProviderWeb}, `{"error":{"code":"subscription:free-usage-exhausted"}}`},
		{"empty body", buildAcc(), ``},
	}
	for _, tc := range cases {
		if ApplyFreeQuotaExhaustion(tc.acc, []byte(tc.body)) {
			t.Fatalf("%s: the response was recorded as a Free refusal", tc.name)
		}
		if !tc.acc.GrokFreeQuota.ConfirmedAt.IsZero() {
			t.Fatalf("%s: the account was modified by an unrecognised response", tc.name)
		}
	}
}

// TestInferFreeProfileDoesNotClaimPaidAccountsAsFree guards the tier projection:
// the Free verdict is what lets the account list label a Build account "Free", so
// it must never fire for an account whose plan string names a paid tier or whose
// billing profile shows a real window.
func TestConsumeSuccessfulQuotaUpdatesSupportedSnapshots(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	web := &store.Account{UsageCurrent: 3, GrokWebQuota: store.GrokWebQuotaSnapshot{
		Auto: store.GrokQuotaWindow{HasRemaining: true, Remaining: 2},
		Fast: store.GrokQuotaWindow{HasRemaining: true, Remaining: 1}, SyncedAt: now,
	}}
	if !ConsumeSuccessfulQuota(web, ProviderWeb, false) {
		t.Fatal("web quota was not consumed")
	}
	if web.UsageCurrent != 2 || web.GrokWebQuota.Auto.Remaining != 1 || web.GrokWebQuota.Fast.Remaining != 1 {
		t.Fatalf("web snapshot not decremented: %+v legacy=%v", web.GrokWebQuota, web.UsageCurrent)
	}

	build := buildAcc()
	build.GrokRateLimits.Requests = store.GrokQuotaWindow{HasRemaining: true, Remaining: 1}
	build.GrokBilling.Weekly = store.GrokQuotaWindow{HasUsage: true, UsagePercent: 99}
	if !ConsumeSuccessfulQuota(build, ProviderBuild, false) || build.GrokRateLimits.Requests.Remaining != 0 {
		t.Fatalf("build request snapshot not decremented: %+v", build.GrokRateLimits)
	}
	if build.GrokBilling.Weekly.UsagePercent != 99 {
		t.Fatal("weekly percentage billing must not be locally decremented")
	}
	if ConsumeSuccessfulQuota(build, ProviderBuild, true) {
		t.Fatal("authoritative response headers must suppress local decrement")
	}
}

func TestInferFreeProfileDoesNotClaimPaidAccountsAsFree(t *testing.T) {
	t.Parallel()

	paid := []string{"supergrok", "XPremium", "x_premium_plus", "heavy", "lite", "Pro", "team", "enterprise"}
	for _, plan := range paid {
		acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: plan}
		if verdict := InferFreeProfile(acc); verdict.Inferred {
			t.Errorf("plan %q was inferred Free", plan)
		}
	}

	// A billing profile with a real window is a paid/entitled account even when
	// the plan string is absent.
	entitled := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: "unknown"}
	entitled.GrokBilling.SyncedAt = time.Now()
	entitled.GrokBilling.Weekly.HasUsage = true
	if verdict := InferFreeProfile(entitled); verdict.Inferred {
		t.Error("an account with a reported weekly window was inferred Free")
	}

	// No evidence at all stays unknown rather than being guessed at.
	unsynced := &store.Account{AccountType: "grok", CredentialType: "oauth"}
	if verdict := InferFreeProfile(unsynced); verdict.Inferred {
		t.Error("an unsynced account was inferred Free")
	}
}

// A Web account's subscription has to come from the mode a limit belongs to:
// the same tier is published as different numbers per mode (auto 150 vs fast
// 400), so a single mixed-mode number cannot classify an account.
func TestApplyWebQuotaInfoClassifiesByModeNotByMixedLimit(t *testing.T) {
	cases := []struct {
		name      string
		autoLimit int64
		fastLimit int64
		want      string
	}{
		{name: "auto heavy with fast basic stays basic", autoLimit: 150, fastLimit: 30, want: "basic"},
		{name: "fast heavy with auto basic stays basic", autoLimit: 7, fastLimit: 400, want: "basic"},
		{name: "both heavy", autoLimit: 150, fastLimit: 400, want: "heavy"},
		{name: "auto super with fast basic", autoLimit: 50, fastLimit: 30, want: "basic"},
		{name: "both super", autoLimit: 50, fastLimit: 140, want: "super"},
		{name: "auto basic only", autoLimit: 20, want: "basic"},
		{name: "fast basic only", fastLimit: 30, want: "basic"},
		{name: "unknown shapes leave the tier alone", autoLimit: 99, fastLimit: 98, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			windows := map[string]*RateLimitInfo{}
			if tc.autoLimit > 0 {
				windows["auto"] = &RateLimitInfo{Limit: tc.autoLimit, HasLimit: true, Remaining: 1, HasRemaining: true}
			}
			if tc.fastLimit > 0 {
				windows["fast"] = &RateLimitInfo{Limit: tc.fastLimit, HasLimit: true, Remaining: 1, HasRemaining: true}
			}
			acc := &store.Account{AccountType: "grok"}
			ApplyWebQuotaInfo(acc, windows)
			if acc.Subscription != tc.want {
				t.Fatalf("subscription=%q want %q", acc.Subscription, tc.want)
			}
		})
	}
}

// inference from a window whose mode is unknown must not invent a paid tier for
// an arbitrary large number, and must leave the lite tier to explicit config.
func TestInferSubscriptionFromRateLimitInfoRequiresKnownShapes(t *testing.T) {
	cases := map[int64]string{
		7: "basic", 20: "basic", 8: "basic", 30: "basic",
		50: "super", 140: "super",
		150: "heavy",
		25: "lite", 70: "lite", 12: "lite",
		1000: "", 151: "", 149: "", 3: "",
	}
	for limit, want := range cases {
		got := inferSubscriptionFromRateLimitInfo(&RateLimitInfo{Limit: limit, HasLimit: true})
		if got != want {
			t.Fatalf("limit=%d subscription=%q want %q", limit, got, want)
		}
	}
}

// The unused heavy mode still identifies the top tier when the upstream sends it.
func TestInferSubscriptionFromWebQuotaHeavyMode(t *testing.T) {
	got := inferSubscriptionFromWebQuota(map[string]*RateLimitInfo{
		"heavy": {Limit: 12, HasLimit: true},
	})
	if got != "heavy" {
		t.Fatalf("subscription=%q want heavy", got)
	}
	if got := inferSubscriptionFromWebQuota(map[string]*RateLimitInfo{
		"heavy": {Limit: 12, HasLimit: true},
		"fast":  {Limit: 30, HasLimit: true},
	}); got != "basic" {
		t.Fatalf("subscription=%q want basic (lowest tier wins)", got)
	}
}
