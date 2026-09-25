package accountpolicy

import (
	"errors"
	"testing"
	"time"

	"orchids-api/internal/store"
)

// qoderBusyError reproduces the two things production actually paired: the text
// Qoder's refusal reached us as, and the retry hint the error advertises.
type qoderBusyError struct {
	message string
	wait    time.Duration
}

func (e qoderBusyError) Error() string             { return e.message }
func (e qoderBusyError) RetryAfter() time.Duration { return e.wait }

// The exact status_message stored for production account 22. The body says
// serviceAvailable:false / retryAfterSeconds:30, but the text blames the
// credential, which is how the pool drained: every account was parked as "429"
// for 30s and the gateway rotated to the next one, which met the same answer.
const productionQoderBusyMessage = `qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p3\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"waitTime\":30}"}`

// TestClassifyGlobalQueueRefusalWaitsWithoutHoldingTheAccount is the regression
// test for the Qoder pool drain.
//
// The contract has two halves. The account must not be held or rotated -- a
// refusal that every account receives cannot be escaped by moving, and parking
// accounts is what emptied the pool. The request must still be retryable, on the
// account it already holds, so a short upstream window becomes a short wait
// instead of a failure.
func TestClassifyGlobalQueueRefusalWaitsWithoutHoldingTheAccount(t *testing.T) {
	acc := &store.Account{ID: 22, AccountType: "qoder", Enabled: true}
	for _, tc := range []struct {
		name    string
		message string
	}{
		{"credential-shaped text carrying 10605", productionQoderBusyMessage},
		{"classified busy form", "qoder gateway is busy: serviceAvailable=false retryAfterSeconds=29"},
		{"upstream pool throttled", "qoder API error: available upstream accounts are rate-limited"},
		{"service unavailable flag alone", `qoder upstream error: status=401, {"serviceAvailable":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := Classify(acc, qoderBusyError{message: tc.message, wait: 30 * time.Second}, "qwen3.8-flash")

			// The account must stay in the pool: no account status, and a scope
			// that is not the account or the credential.
			if verdict.Status != "" {
				t.Errorf("account status = %q, want none; a shared refusal is not this account's fault", verdict.Status)
			}
			if verdict.Scope == ScopeAccount || verdict.Scope == ScopeCredential {
				t.Errorf("scope = %q, want a scope that does not hold the account", verdict.Scope)
			}
			// Nothing is persisted. A model cooldown here would take this account,
			// and then every other one, out of selection for the window, which is
			// the fail-fast this replaces.
			if verdict.Cooldown != 0 {
				t.Errorf("cooldown = %v, want 0 so no account or model state is written", verdict.Cooldown)
			}
			// Wait and retry, on the account already held.
			if !verdict.Retryable {
				t.Error("Retryable = false; a short queue window should be waited out, not failed")
			}
			if verdict.SwitchAccount {
				t.Error("SwitchAccount = true; rotating multiplies one shared refusal across every account")
			}
			if verdict.Model != "qwen3.8-flash" {
				t.Errorf("model = %q, want the reported model", verdict.Model)
			}
		})
	}
}

// TestClassifyAccountScopedRateLimitStillRotates guards the other direction: a
// genuine per-account throttle must keep its account scope and still switch, so
// the global-refusal rule cannot swallow the channels it does not describe.
func TestClassifyAccountScopedRateLimitStillRotates(t *testing.T) {
	acc := &store.Account{ID: 3, AccountType: "cline", Enabled: true}
	verdict := Classify(acc, retryAfterTestError{wait: 2 * time.Minute}, "claude-sonnet-4")

	if verdict.Status != "429" {
		t.Errorf("status = %q, want 429 for a real per-account cap", verdict.Status)
	}
	if verdict.Scope != ScopeAccount {
		t.Errorf("scope = %q, want account", verdict.Scope)
	}
	if !verdict.SwitchAccount || !verdict.Retryable {
		t.Error("a genuine per-account throttle must still switch accounts and retry")
	}
	if verdict.Cooldown != 2*time.Minute {
		t.Errorf("cooldown = %v, want the upstream hint", verdict.Cooldown)
	}
}

// TestClassifyGlobalRefusalIgnoresAnUnusableHint pins that a shared refusal does
// not derive any account state from the hint. The wait itself is bounded where
// it is actually spent (the handler caps an honoured retry-after), so nothing
// here may invent a cooldown from a missing or absurd value.
func TestClassifyGlobalRefusalIgnoresAnUnusableHint(t *testing.T) {
	acc := &store.Account{ID: 22, AccountType: "qoder", Enabled: true}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no hint at all", errors.New("qoder gateway is busy")},
		{"zero hint", qoderBusyError{message: "qoder gateway is busy", wait: 0}},
		{"absurd hint", qoderBusyError{message: "qoder gateway is busy", wait: 12 * time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := Classify(acc, tc.err, "qwen3.8-flash")
			if verdict.Cooldown != 0 {
				t.Errorf("cooldown = %v, want 0 regardless of the hint", verdict.Cooldown)
			}
			if verdict.Status != "" {
				t.Errorf("status = %q, want none", verdict.Status)
			}
			if !verdict.Retryable || verdict.SwitchAccount {
				t.Errorf("retryable=%v switch=%v, want a wait on the same account", verdict.Retryable, verdict.SwitchAccount)
			}
		})
	}
}
