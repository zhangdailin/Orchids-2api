package accountpolicy

import (
	"errors"
	"strings"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

func grokSSO() *store.Account {
	return &store.Account{ID: 1, AccountType: "grok", CredentialType: "sso", GrokProvider: "web", ClientCookie: "sso=t"}
}

// TestClassify_RefusedCredentialNeedsLogin pins the verdict for a cookie the
// upstream refused: the account is held, the operator is told to re-login, and
// the reason is persisted together with the status.
func TestClassify_RefusedCredentialNeedsLogin(t *testing.T) {
	acc := grokSSO()
	v := Classify(acc, errors.New("401: grok session unauthenticated"), "grok-4.6")

	if v.Scope != ScopeCredential {
		t.Fatalf("scope = %q, want %q", v.Scope, ScopeCredential)
	}
	if !v.NeedsLogin || (v.Scope != ScopeAccount && v.Scope != ScopeCredential) {
		t.Fatalf("verdict must require a login and hold the account: %+v", v)
	}
	if v.Status != "401" || v.Message == "" {
		t.Fatalf("verdict must carry status+reason: %+v", v)
	}
	if v.Cooldown != CredentialReverify {
		t.Fatalf("cooldown = %v, want %v", v.Cooldown, CredentialReverify)
	}
	v.Apply(acc)
	if acc.AuthStatus != store.AccountAuthStatusReauthRequired {
		t.Fatalf("auth status = %q, want reauthRequired", acc.AuthStatus)
	}
}

// TestClassify_ModelScopedFailureKeepsAccount covers the P0 rule: a complaint
// about one model must not take the whole account out of the pool.
func TestClassify_ModelScopedFailureKeepsAccount(t *testing.T) {
	acc := grokSSO()
	for _, message := range []string{
		"workbuddy API error: status=200, code=6004, message=usage exceeds frequency limit",
		"qoder agent limit reached; resets at 2026-09-27T19:47:13Z",
		`qoder upstream rejected the credential: {"agentLimitResetTime":1790538433100}`,
		"404: model is not found",
		"model not found: grok-4.6",
		"no_implementation_available for grok-4.6",
		"context_window_exceeded",
		"requested base model not allowed for this account",
	} {
		v := Classify(acc, errors.New(message), "grok-4.6")
		if v.Scope != ScopeModel {
			t.Fatalf("%q: scope = %q, want %q", message, v.Scope, ScopeModel)
		}
		if v.Status != "" {
			t.Fatalf("%q: a model-scoped failure must not set an account status, got %q", message, v.Status)
		}
		if v.Scope == ScopeAccount || v.Scope == ScopeCredential {
			t.Fatalf("%q: a model-scoped failure must not hold the account", message)
		}
		if v.Model != "grok-4.6" {
			t.Fatalf("%q: model = %q, want the reported model", message, v.Model)
		}
	}
}

// TestClassify_RateLimitIsAccountScopedWithShortCooldown keeps throttling a
// temporary, account-wide condition.
func TestClassify_RateLimitIsAccountScopedWithShortCooldown(t *testing.T) {
	v := Classify(grokSSO(), errors.New("429: too many requests"), "grok-4.6")
	if v.Scope != ScopeAccount || v.Status != "429" {
		t.Fatalf("verdict = %+v, want account-scoped 429", v)
	}
	if v.Cooldown != CooldownRateLimit {
		t.Fatalf("cooldown = %v, want %v", v.Cooldown, CooldownRateLimit)
	}
	if v.NeedsLogin {
		t.Fatal("throttling must not require a login")
	}
}

// TestClassify_SuccessStampsVerdict keeps "never checked" distinguishable from
// "checked and healthy": the success verdict must stamp VerifiedAt.
func TestClassify_SuccessStampsVerdict(t *testing.T) {
	acc := grokSSO()
	verdict := Classify(acc, nil, "grok-4.6")
	if verdict.Status != "" || (verdict.Scope != ScopeNone && verdict.Scope != ScopeModel) {
		t.Fatalf("success verdict is not healthy: %+v", verdict)
	}
	verdict.Apply(acc)
	if acc.VerifiedAt.IsZero() {
		t.Fatal("a success verdict must stamp VerifiedAt")
	}
	if acc.StatusCode != "" || acc.StatusMessage != "" {
		t.Fatalf("a success verdict must clear status/reason: %+v", acc)
	}
}

// TestApply_KeepsStatusAndReasonTogether is the invariant the account table
// depends on: a reason never outlives its status.
func TestApply_KeepsStatusAndReasonTogether(t *testing.T) {
	acc := grokSSO()
	acc.StatusCode = "429"
	acc.StatusMessage = "old reason"
	acc.LastAttempt = time.Now().Add(-time.Hour)

	Success(time.Now()).Apply(acc)
	if acc.StatusCode != "" || acc.StatusMessage != "" {
		t.Fatalf("recovery left %q / %q", acc.StatusCode, acc.StatusMessage)
	}
	if !acc.LastAttempt.IsZero() {
		t.Fatalf("recovery must clear the attempt stamp, got %v", acc.LastAttempt)
	}

	Classify(acc, errors.New("429: slow down"), "").Apply(acc)
	if acc.StatusCode != "429" || acc.StatusMessage == "" {
		t.Fatalf("failure verdict lost status/reason: %+v", acc)
	}
	if acc.LastAttempt.IsZero() {
		t.Fatal("failure verdict must anchor the cooldown")
	}
}

// TestAccountLifecycle pins hold/expiry behaviour shared by pool and scheduler.
func TestAccountLifecycle(t *testing.T) {
	rejected := grokSSO()
	Classify(rejected, errors.New("401: grok session unauthenticated"), "").Apply(rejected)
	now := rejected.VerifiedAt
	if !AccountHeld(rejected, now) {
		t.Fatal("a freshly rejected credential must hold the account")
	}
	if NeedsReverify(rejected, now.Add(time.Minute)) {
		t.Fatal("a refused credential must not be re-asked inside the revertify window")
	}
	if !NeedsReverify(rejected, now.Add(CredentialReverify)) {
		t.Fatal("the credential must be re-asked once its window is over")
	}
	if !NeedsReverify(&store.Account{StatusCode: "401"}, now) {
		t.Fatal("a 401 without a verdict stamp must be due immediately")
	}
	if AccountHeld(rejected, now.Add(24*time.Hour)) == false {
		t.Fatal("reauthRequired must remain held until a successful re-authentication")
	}

	healthy := grokSSO()
	Success(now).Apply(healthy)
	if AccountHeld(healthy, now) {
		t.Fatal("a healthy account must never be held")
	}
	if !NeedsFirstVerdict(grokSSO()) {
		t.Fatal("an account with no verdict needs one")
	}
	if NeedsFirstVerdict(healthy) {
		t.Fatal("a verified account does not need a first verdict")
	}
}

// TestCooldownFor_MatchesPoolValues guards the numbers the pool already relies on.
func TestCooldownFor_MatchesPoolValues(t *testing.T) {
	cases := []struct {
		acc  *store.Account
		want time.Duration
	}{
		{&store.Account{StatusCode: "401"}, 30 * time.Minute},
		{&store.Account{StatusCode: "429"}, 30 * time.Second},
		{&store.Account{StatusCode: "402"}, 24 * time.Hour},
		{&store.Account{StatusCode: "402", AccountType: "puter"}, 15 * time.Minute},
		// A WorkBuddy account reaches status 402 only when its allowance is gone
		// (a model-scoped refusal writes no status), so it is held like any other
		// payment verdict — and released early by isAccountAvailable once
		// QuotaResetAt says the allowance is back.
		{&store.Account{StatusCode: "402", AccountType: "workbuddy"}, 24 * time.Hour},
		{&store.Account{StatusCode: "403"}, 24 * time.Hour},
		{&store.Account{StatusCode: "403", AccountType: "grok"}, 10 * time.Minute},
		{&store.Account{StatusCode: "weird"}, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := CooldownFor(tc.acc); got != tc.want {
			t.Fatalf("CooldownFor(%s/%s) = %v, want %v", tc.acc.AccountType, tc.acc.StatusCode, got, tc.want)
		}
	}
}

func TestRateLimitCooldownIsBoundedExponential(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	failures := []int{1, 2, 3, 6, 7, 20}
	for i, failureCount := range failures {
		if got := RateLimitCooldown(failureCount); got != want[i] {
			t.Fatalf("RateLimitCooldown(%d)=%v want %v", failureCount, got, want[i])
		}
	}
	if got := BoundRateLimitCooldown(2 * time.Hour); got != 30*time.Minute {
		t.Fatalf("bounded retry-after=%v want 30m", got)
	}
}

func TestAccountHeldUsesLaterBoundedReset(t *testing.T) {
	now := time.Now()
	acc := &store.Account{StatusCode: "429", LastAttempt: now, RateLimitFailures: 1, QuotaResetAt: now.Add(10 * time.Minute)}
	if !AccountHeld(acc, now.Add(time.Minute)) {
		t.Fatal("quota reset later than exponential cooldown must keep account held")
	}
	if AccountHeld(acc, now.Add(11*time.Minute)) {
		t.Fatal("account should recover after the later reset")
	}
}

// TestAccountHeld_429IgnoresBillingCycleReset is the regression test for the
// WorkBuddy outage of 2026-09-21.
//
// The quota sync writes the free plan's billing-cycle end into the same
// QuotaResetAt field a throttle uses for its retry-after, so a single one-minute
// 429 ("code=14003 too many requests") was read as "hold this account until the
// cycle resets" — days away. Every WorkBuddy account that hit one 429 left the
// pool for the rest of the month, the channel answered 503 to every request, and
// the log showed only eighteen 429s against 116 failures. A rate limit is a short
// capacity problem, so its hold is capped at the same ceiling RateLimitCooldown
// uses; a genuinely longer 402 allowance verdict is untouched.
func TestAccountHeld_429IgnoresBillingCycleReset(t *testing.T) {
	now := time.Now()
	cycleEnd := now.Add(9 * 24 * time.Hour) // the free-plan boundary the upstream reports

	acc := &store.Account{
		AccountType:       "workbuddy",
		StatusCode:        "429",
		LastAttempt:       now,
		RateLimitFailures: 2,
		QuotaResetAt:      cycleEnd,
	}

	if !AccountHeld(acc, now.Add(time.Minute)) {
		// One minute is well inside both the exponential cooldown and the cap.
		t.Fatal("a freshly rate-limited account must stay held during its cooldown")
	}
	if AccountHeld(acc, now.Add(CooldownRateLimitMax+time.Minute)) {
		t.Fatalf("429 held for %v; a billing-cycle reset must not extend a rate limit past %v",
			9*24*time.Hour, CooldownRateLimitMax)
	}
	if AccountHeld(acc, now.Add(2*time.Hour)) {
		t.Fatal("account should be back in rotation within the rate-limit ceiling")
	}

	// The same far-future reset on a 402 is a real allowance verdict: it must keep
	// the account parked, or a spent account is offered again on every request.
	spent := &store.Account{
		AccountType:  "workbuddy",
		StatusCode:   "402",
		LastAttempt:  now,
		QuotaResetAt: cycleEnd,
	}
	if !AccountHeld(spent, now.Add(2*time.Hour)) {
		t.Fatal("a spent allowance must still hold the account until its reset")
	}
}

// TestAccountHeld_429KeepsShortRetryAfter pins the other direction: the ceiling
// bounds an over-long reset, it does not replace a shorter one the upstream
// actually stated.
func TestAccountHeld_429KeepsShortRetryAfter(t *testing.T) {
	now := time.Now()
	acc := &store.Account{StatusCode: "429", LastAttempt: now, RateLimitFailures: 1, QuotaResetAt: now.Add(20 * time.Minute)}
	if !AccountHeld(acc, now.Add(10*time.Minute)) {
		t.Fatal("a stated 20m retry-after must still hold the account past its 30s exponential cooldown")
	}
	if AccountHeld(acc, now.Add(21*time.Minute)) {
		t.Fatal("account should recover once the stated retry-after passed")
	}
}

// TestClassify_WorkBuddyPaymentRefusalIsModelScoped pins the reported behaviour:
// WorkBuddy's free models keep working once the metered credit package is spent,
// so a payment refusal must cool down only the model that was asked for instead
// of parking the whole account (and its free models) for the 24h payment cooldown.
func TestClassify_WorkBuddyPaymentRefusalIsModelScoped(t *testing.T) {
	acc := &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true}
	verdict := Classify(acc, errors.New("workbuddy API error: status=402 message=insufficient credits for model"), "claude-sonnet-4.5")

	if verdict.Scope != ScopeModel || verdict.Model != "claude-sonnet-4.5" {
		t.Fatalf("verdict = %+v, want a model-scoped cooldown", verdict)
	}
	if verdict.Status != "" {
		t.Fatalf("status = %q, want no account status for a spent credit package", verdict.Status)
	}
	if verdict.Cooldown <= 0 || verdict.Cooldown >= CooldownPayment {
		t.Fatalf("cooldown = %v, want the short model window", verdict.Cooldown)
	}
	verdict.Apply(acc)
	if acc.StatusCode != "" {
		t.Fatalf("StatusCode = %q, want the account left schedulable", acc.StatusCode)
	}
	if AccountHeld(acc, time.Now()) {
		t.Fatal("a WorkBuddy payment refusal must not hold the account")
	}
	// Without a model to name there is nothing to cool down, and the account must
	// still not be parked.
	anonymous := Classify(&store.Account{AccountType: "workbuddy"}, errors.New("status=402 insufficient credits"), "")
	if anonymous.Status != "" || AccountHeld(&store.Account{AccountType: "workbuddy", StatusCode: anonymous.Status}, time.Now()) {
		t.Fatalf("anonymous verdict = %+v, want the account left alone", anonymous)
	}
}

// TestCredentialMessageIsProviderAware keeps the operator instruction concrete.
func TestCredentialMessageIsProviderAware(t *testing.T) {
	grokVerdict := Classify(grokSSO(), errors.New("401: unauthenticated"), "")
	if !strings.Contains(grokVerdict.Message, "重新登录") {
		t.Fatalf("grok reason = %q", grokVerdict.Message)
	}
	other := Classify(&store.Account{AccountType: "warp"}, errors.New("401: expired"), "")
	if other.Message == "" || other.NeedsLogin == false {
		t.Fatalf("warp verdict = %+v", other)
	}
}

// TestClassify_WorkBuddyCreditExhaustionParksTheAccount is the regression test for
// the outage the model-scoped rule produced.
//
// The upstream's real refusal for a spent allowance is code 14018, whose text is
// "Credits exhausted. Please visit the link below to purchase add-on packs". That
// is a fact about the whole account — it is returned for every model — but it was
// read as a model-scoped payment refusal, so the account stayed in rotation, every
// request retried the whole pool, and the account table carried no reason for it.
func TestClassify_WorkBuddyCreditExhaustionParksTheAccount(t *testing.T) {
	// The production message, verbatim in shape: the upstream wraps it in JSON and
	// the transport wraps that in a status.
	production := `workbuddy API error: status=429, message={"error":{"data":{"code":14018,` +
		`"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and ` +
		`get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"abc"}}}`

	acc := &store.Account{ID: 1, AccountType: "workbuddy", Enabled: true}
	verdict := Classify(acc, errors.New(production), "fast-model")

	if verdict.Scope != ScopeAccount {
		t.Fatalf("scope = %v, want an account-scoped verdict: an exhausted allowance refuses every model", verdict.Scope)
	}
	if verdict.Status != "402" {
		t.Fatalf("status = %q, want 402 so the account table can explain the account", verdict.Status)
	}
	verdict.Apply(acc)
	if !AccountHeld(acc, time.Now()) {
		t.Fatal("a credit-exhausted account must be held, or every request retries it")
	}
	// The reason reaches the operator, including what to do about it.
	if !strings.Contains(acc.StatusMessage, "codebuddy.ai/profile/usage") {
		t.Fatalf("status message = %q, want the upstream's purchase link", acc.StatusMessage)
	}
}

// TestIsCreditExhaustion_SeparatesTheTwoRefusals pins the distinction the rule
// rests on: both refusals arrive as 402, and only the wording says which one is
// about the account rather than about one request.
func TestIsCreditExhaustion_SeparatesTheTwoRefusals(t *testing.T) {
	exhausted := []string{
		"workbuddy API error: status=429, message=...Credits exhausted. Please visit the link below...",
		"status=402 no AI credits remaining",
		"available funding is insufficient to complete this request",
		"402 out of credits",
	}
	for _, message := range exhausted {
		if !apperrors.IsCreditExhaustion(message) {
			t.Errorf("IsCreditExhaustion(%q) = false, want true", message)
		}
	}
	modelScoped := []string{
		"workbuddy API error: status=402 message=insufficient credits for model",
		"status=402 message=this model requires a paid plan",
		"workbuddy API error: status=429, code=14003, message=too many requests",
	}
	for _, message := range modelScoped {
		if apperrors.IsCreditExhaustion(message) {
			t.Errorf("IsCreditExhaustion(%q) = true, want false", message)
		}
	}
}
