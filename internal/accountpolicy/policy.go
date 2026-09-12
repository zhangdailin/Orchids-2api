// Package accountpolicy centralises how one upstream result becomes an account
// state change. Before this existed the same question — "is this account still
// usable, and for how long?" — was answered independently by the scheduler, the
// admin API and the account pool, which is how an account could look healthy in
// one place and unauthorized in another.
package accountpolicy

import (
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

// Scope states how much of an account a failure invalidates.
type Scope string

const (
	// ScopeNone: the result does not change account state at all (a model
	// mismatch, a malformed request, a client-side error).
	ScopeNone Scope = "none"
	// ScopeModel: only the model that produced the result is exhausted or
	// throttled; the account stays usable for its other models.
	ScopeModel Scope = "model"
	// ScopeAccount: the account as a whole is temporarily unavailable (rate
	// limit, quota, transient upstream failure).
	ScopeAccount Scope = "account"
	// ScopeCredential: the credential itself was refused. The account cannot
	// recover by waiting; it needs a new login or a new cookie.
	ScopeCredential Scope = "credential"
)

// Cooldown windows. They mirror the account pool's long-standing values so the
// scheduler and the pool expire a verdict at the same moment.
const (
	CooldownAuth       = 5 * time.Minute
	CooldownRateLimit  = 1 * time.Minute
	CooldownPayment    = 24 * time.Hour
	CooldownPuterQuota = 15 * time.Minute
	CooldownBlocked    = 24 * time.Hour
	CooldownBlockedGro = 10 * time.Minute
	CooldownTransient  = 5 * time.Minute

	// CredentialReverify is how long a credential the upstream refused is kept
	// out of the refresh rotation before it is re-asked once, in case the
	// rejection was transient or the operator re-authenticated.
	CredentialReverify = 30 * time.Minute
)

// Verdict is the complete, self-describing outcome of one upstream result.
type Verdict struct {
	// Status is the account status code to persist ("" clears it).
	Status string
	// Message is the operator-facing reason. It never outlives the status.
	Message string
	// Scope says what the failure invalidates.
	Scope Scope
	// Retryable reports whether the same request may be attempted again.
	Retryable bool
	// SwitchAccount reports whether the pool should try a different account.
	SwitchAccount bool
	// NeedsLogin reports that waiting cannot help: a human must re-authorize.
	NeedsLogin bool
	// Cooldown is how long the account (or the model) stays withheld.
	Cooldown time.Duration
	// Model names the model the verdict is scoped to, when Scope is ScopeModel.
	Model string
	// At is when the result was observed; it anchors the cooldown.
	At time.Time
}

// HoldsAccount reports whether the verdict keeps the whole account out of the pool.
func (v Verdict) HoldsAccount() bool {
	return v.Scope == ScopeAccount || v.Scope == ScopeCredential
}

// Healthy reports whether the result left the account usable.
func (v Verdict) Healthy() bool {
	return v.Status == "" && (v.Scope == ScopeNone || v.Scope == ScopeModel)
}

// Success is the verdict for an upstream result that proved the credential
// works. It always stamps VerifiedAt so "never checked" stays distinguishable
// from "checked and healthy".
func Success(at time.Time) Verdict {
	return Verdict{Scope: ScopeNone, At: at}
}

// Apply records the verdict on the account. It is the only place that moves the
// status, the reason and the verdict stamp together, so a partial write can
// never leave a reason behind without its status, or a stale status in place of
// a fresh one.
func (v Verdict) Apply(acc *store.Account) {
	if acc == nil {
		return
	}
	at := v.At
	if at.IsZero() {
		at = time.Now()
	}
	acc.StatusCode = strings.TrimSpace(v.Status)
	if acc.StatusCode == "" {
		acc.StatusMessage = ""
		acc.LastAttempt = time.Time{}
	} else {
		acc.StatusMessage = strings.TrimSpace(v.Message)
		acc.LastAttempt = at
	}
	// Any verdict — healthy or not — proves the credential was exercised, which
	// is what makes "never checked" distinguishable from "checked and healthy".
	acc.VerifiedAt = at
}

// ClearCredentialVerdict drops the status and the verdict stamp because the
// credential they described has been replaced.
func ClearCredentialVerdict(acc *store.Account) {
	if acc == nil {
		return
	}
	acc.StatusCode = ""
	acc.StatusMessage = ""
	acc.LastAttempt = time.Time{}
	acc.VerifiedAt = time.Time{}
	acc.ClearVerifiedAt = true
}

// ScopeForStatus maps an already-classified status code to the part of the
// account it invalidates, so callers that only have the code (a verifier that
// returned "429", an admin token probe) still produce a complete verdict.
func ScopeForStatus(status string) Scope {
	switch strings.TrimSpace(status) {
	case "401":
		return ScopeCredential
	case "402", "403", "404", "429":
		return ScopeAccount
	case "":
		return ScopeNone
	default:
		return ScopeAccount
	}
}

// Retryable derives whether the same request may be attempted again from the
// shared upstream-error classification. It exists so the request path and the
// scheduler read one rule instead of two: before this, the handler decided
// retries from category strings while the scheduler decided state from the
// policy, and the two could disagree about the same error.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	return apperrors.ClassifyUpstreamError(err.Error()).Retryable
}

// Cancelled reports whether the failure was the caller going away. A cancelled
// request must not be retried, must not cool the account down, and must not be
// reported as an upstream fault.
func Cancelled(err error) bool {
	if err == nil {
		return false
	}
	class := apperrors.ClassifyUpstreamError(err.Error())
	return class.Category == "canceled"
}

// Classify turns an upstream error into a verdict.
func Classify(acc *store.Account, err error, model string) Verdict {
	if err == nil {
		return Success(time.Now())
	}
	message := strings.TrimSpace(err.Error())
	lower := strings.ToLower(message)
	now := time.Now()

	// A model-scoped complaint must not take the account out of service: the
	// other models of the same account remain usable.
	if isModelScopedFailure(lower) {
		return Verdict{
			Scope:         ScopeModel,
			Message:       message,
			Model:         model,
			Retryable:     Retryable(err),
			SwitchAccount: true,
			Cooldown:      CooldownRateLimit,
			At:            now,
		}
	}

	switch apperrors.ClassifyAccountStatus(message) {
	case "401":
		return Verdict{
			Status:     "401",
			Message:    credentialMessage(acc, message),
			Scope:      ScopeCredential,
			Retryable:  Retryable(err),
			NeedsLogin: true,
			Cooldown:   CredentialReverify,
			At:         now,
		}
	case "403", "404":
		cooldown := CooldownBlocked
		if isGrok(acc) {
			// Grok answers 403 for Cloudflare challenges, which clear quickly.
			cooldown = CooldownBlockedGro
		}
		return Verdict{
			Status: apperrors.ClassifyAccountStatus(message), Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: cooldown, At: now,
		}
	case "402":
		cooldown := CooldownPayment
		if strings.EqualFold(strings.TrimSpace(accountType(acc)), "puter") {
			cooldown = CooldownPuterQuota
		}
		return Verdict{
			Status: "402", Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: cooldown, At: now,
		}
	case "429":
		return Verdict{
			Status: "429", Message: message,
			Scope: ScopeAccount, Retryable: Retryable(err), SwitchAccount: true,
			Cooldown: CooldownRateLimit, At: now,
		}
	}

	// Everything else is transient: keep the account, retry elsewhere.
	return Verdict{
		Status: "", Message: "",
		Scope: ScopeNone, Retryable: Retryable(err), SwitchAccount: true,
		At: now,
	}
}

// isModelScopedFailure reports whether the message blames a model rather than
// the credential or the account's quota.
func isModelScopedFailure(lower string) bool {
	return strings.Contains(lower, "model is not found") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "no_implementation_available") ||
		strings.Contains(lower, "context_window_exceeded") ||
		strings.Contains(lower, "max_token_limit") ||
		strings.Contains(lower, "model unavailable") ||
		strings.Contains(lower, "requested base model")
}

// credentialMessage explains a refused credential in operator terms. The action
// must be concrete: waiting cannot repair a credential the upstream retired.
func credentialMessage(acc *store.Account, fallback string) string {
	switch {
	case isGrok(acc):
		return "上游拒绝该 SSO Cookie（会话已失效，或被同账号的另一次登录替换），请重新登录该 xAI 账号并抓取新的 Cookie"
	case acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "workbuddy"):
		return "上游拒绝该 WorkBuddy 授权，请在账号管理中重新完成 OAuth 登录"
	default:
		return "上游拒绝该凭据，需要重新登录后重试"
	}
}

func isGrok(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok")
}

func accountType(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return acc.AccountType
}

// AccountHeld reports whether the account status is still within its cooldown.
// It is the one place the pool and the scheduler ask "may I use this account?".
func AccountHeld(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return false
	}
	if acc.LastAttempt.IsZero() {
		return true
	}
	return now.Sub(acc.LastAttempt) < CooldownFor(acc)
}

// CooldownFor returns the cooldown the account's current status implies.
func CooldownFor(acc *store.Account) time.Duration {
	if acc == nil {
		return CooldownTransient
	}
	switch strings.TrimSpace(acc.StatusCode) {
	case "401":
		return CooldownAuth
	case "429":
		return CooldownRateLimit
	case "402":
		if strings.EqualFold(strings.TrimSpace(accountType(acc)), "puter") {
			return CooldownPuterQuota
		}
		return CooldownPayment
	case "403", "404":
		if isGrok(acc) {
			return CooldownBlockedGro
		}
		return CooldownBlocked
	case store.AccountStatusWarpQuotaExhausted:
		return CooldownTransient
	default:
		return CooldownTransient
	}
}

// NeedsReverify reports whether a credential the upstream refused is due to be
// re-asked. Until CredentialReverify has elapsed the answer is no: re-asking a
// refused credential on every tick consumes the budget healthy accounts need.
// The operator installing a new credential clears the verdict stamp, which makes
// the account due immediately.
func NeedsReverify(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.StatusCode) != "401" {
		return false
	}
	if acc.VerifiedAt.IsZero() {
		return true
	}
	return now.Sub(acc.VerifiedAt) >= CredentialReverify
}

// NeedsFirstVerdict reports whether an account has never been checked, so the
// scheduler must give it one before any pool decision trusts it.
func NeedsFirstVerdict(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	return strings.TrimSpace(acc.StatusCode) == "" && acc.VerifiedAt.IsZero()
}
