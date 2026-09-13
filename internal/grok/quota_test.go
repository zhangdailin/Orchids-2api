package grok

import (
	"testing"
	"time"

	"orchids-api/internal/store"
)

// buildAcc is a Build OAuth account, the only provider the Build Free window (and
// therefore this parser) applies to.
func buildAcc() *store.Account {
	return &store.Account{ID: 143, AccountType: "grok", GrokProvider: ProviderBuild}
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
