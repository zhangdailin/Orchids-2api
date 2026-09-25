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

// TestClassifyGlobalQueueRefusalDoesNotHoldTheAccount is the regression test for
// the Qoder pool drain.
func TestClassifyGlobalQueueRefusalDoesNotHoldTheAccount(t *testing.T) {
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
			// Neither retrying nor rotating can help: every account is told the
			// same thing, so the request must fail fast instead of burning the pool.
			if verdict.Retryable {
				t.Error("Retryable = true; the next attempt meets the identical refusal")
			}
			if verdict.SwitchAccount {
				t.Error("SwitchAccount = true; rotating multiplies one shared refusal across every account")
			}
			// The upstream hint is still honoured so the model is withheld for it.
			if verdict.Cooldown != 30*time.Second {
				t.Errorf("cooldown = %v, want the upstream's 30s hint", verdict.Cooldown)
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

// TestClassifyGlobalRefusalWithoutHintIsStillBounded pins that a refusal with no
// usable hint does not inherit an unbounded or zero cooldown.
func TestClassifyGlobalRefusalWithoutHintIsStillBounded(t *testing.T) {
	acc := &store.Account{ID: 22, AccountType: "qoder", Enabled: true}
	verdict := Classify(acc, errors.New("qoder gateway is busy"), "qwen3.8-flash")
	if verdict.Cooldown != CooldownRateLimit {
		t.Errorf("cooldown = %v, want the default rate-limit cooldown %v", verdict.Cooldown, CooldownRateLimit)
	}
	if verdict.SwitchAccount {
		t.Error("SwitchAccount = true; a shared refusal must not rotate")
	}
}
