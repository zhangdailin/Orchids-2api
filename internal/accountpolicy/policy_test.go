package accountpolicy

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	if AccountHeld(rejected, now.Add(CooldownFor(rejected)+time.Minute)) {
		t.Fatal("the pool cooldown must expire")
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
		{&store.Account{StatusCode: "401"}, 5 * time.Minute},
		{&store.Account{StatusCode: "429"}, 1 * time.Minute},
		{&store.Account{StatusCode: "402"}, 24 * time.Hour},
		{&store.Account{StatusCode: "402", AccountType: "puter"}, 15 * time.Minute},
		// WorkBuddy's free models survive a spent credit package, so a payment
		// verdict never holds the account: zero cooldown keeps it in rotation and
		// releases a marker persisted before that rule existed.
		{&store.Account{StatusCode: "402", AccountType: "workbuddy"}, 0},
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
