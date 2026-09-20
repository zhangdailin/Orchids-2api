package handler

import (
	"net/http"
	"strings"

	apperrors "orchids-api/internal/errors"
)

// The client-facing text for a request the account pool cannot take. They are
// constants because the same condition must read the same way wherever it is
// reported, and because the pool's own note ("all matching accounts are cooling
// down for the requested model") is a diagnostic that names internal state — it
// belongs in the log, not in a body a client may show to a user.
const (
	// poolAllowanceMessage answers an exhausted allowance: waiting is not enough,
	// somebody has to add credits or capacity.
	poolAllowanceMessage = "Request failed: every account for this channel has exhausted its allowance. Add credits or accounts, or wait for the quota reset."
	// poolQoderModelMessage answers Qoder's business rate limit, which names the
	// channel on purpose: unlike a generic capacity problem, the fix is to pick
	// another model or wait for that model's window.
	poolQoderModelMessage = "Request failed: the requested Qoder model is temporarily rate-limited. Please retry after its cooldown or choose another model."
	// poolModelCooldownMessage is the channel-neutral form of the same answer.
	poolModelCooldownMessage = "Request failed: the requested model is cooling down on this channel. Please retry after its cooldown or choose another model."
	// poolRateLimitedMessage answers a pool every account of which is cooling down.
	poolRateLimitedMessage = "Request failed: all available accounts for this channel are currently rate-limited. Please wait for cooldown or add another valid account."
	// poolBusyMessage answers a pool whose accounts are all serving other requests.
	poolBusyMessage = "Request failed: every account for this channel is busy with other requests. Please retry shortly."
	// poolModelUnavailableMessage answers a request for a model the channel's
	// accounts cannot route (Warp's per-account model choices).
	poolModelUnavailableMessage = "Request failed: the requested model is not available on this channel's accounts. Choose another model or add an account that supports it."
	// poolNoAccountsMessage answers the residual cases (no accounts, or accounts
	// that cannot serve at all). The client can do nothing about them, so the
	// answer points at the operator instead of at the request.
	poolNoAccountsMessage = "Request failed: no account in this channel can serve the request. Please check the account pool in Admin UI or add valid accounts."
	// poolRetriesExhaustedMessage is the mid-request default: attempts were made
	// and every one of them failed.
	poolRetriesExhaustedMessage = "Request failed: retries exhausted and no available accounts. Please check account statuses in Admin UI or add valid accounts."
)

// poolExhaustion is how a request that no account could take is answered.
//
// Category drives both the status and the error type, through
// apperrors.StatusForCategory, so the status a client sees and the text it reads
// cannot disagree. An empty category means "nothing specific could be said": the
// caller supplies the default for its entrance.
type poolExhaustion struct {
	category string
	message  string
}

// classifyPoolExhaustion maps "no account in this channel could take this
// request" onto the answer the client gets.
//
// Both entrances share it — the initial selection in Handler.HandleMessages and
// the retry-exhausted path in streamHandler.InjectNoAvailableAccountError —
// because they used to disagree about the same condition. The retry path already
// answered a cooling or exhausted pool with a retryable 429; the initial
// selection answered it with 503 "overloaded_error" carrying the pool's internal
// note, which is how a capacity problem reached the caller as a server fault.
// selectErr is the pool's own error (its parenthetical says why the pool is
// empty); lastErr is an upstream error when there is one, which is the more
// specific answer when it is present.
func classifyPoolExhaustion(selectErr error, lastErr string) poolExhaustion {
	lowerLastErr := strings.ToLower(strings.TrimSpace(lastErr))
	lowerSelect := ""
	if selectErr != nil {
		lowerSelect = strings.ToLower(selectErr.Error())
	}
	switch {
	case apperrors.IsCreditExhaustion(lowerLastErr) || strings.Contains(lowerSelect, "exhausted their allowance"):
		return poolExhaustion{category: "quota_exhausted", message: poolAllowanceMessage}
	case strings.Contains(lowerLastErr, "qoder agent limit reached") ||
		strings.Contains(lowerLastErr, "qoder model rate limited") ||
		strings.Contains(lowerLastErr, "model cooldown"):
		return poolExhaustion{category: "rate_limit", message: poolQoderModelMessage}
	case strings.Contains(lowerSelect, "cooling down for the requested model"):
		return poolExhaustion{category: "rate_limit", message: poolModelCooldownMessage}
	case strings.Contains(lowerSelect, "rate-limited or cooling down"):
		return poolExhaustion{category: "rate_limit", message: poolRateLimitedMessage}
	case strings.Contains(lowerSelect, "concurrency limit"):
		return poolExhaustion{category: "rate_limit", message: poolBusyMessage}
	case strings.Contains(lowerSelect, "is not available in the current") && strings.Contains(lowerSelect, "account pool"):
		return poolExhaustion{category: "model_unavailable", message: poolModelUnavailableMessage}
	case apperrors.ClassifyUpstreamError(lastErr).Category == "rate_limit":
		return poolExhaustion{category: "rate_limit", message: poolRateLimitedMessage}
	}
	return poolExhaustion{}
}

// writePoolExhaustion answers a request the pool could not take at all. It is the
// initial-selection entrance's answer, where nothing has been committed yet.
func writePoolExhaustion(w http.ResponseWriter, out poolExhaustion) {
	if strings.TrimSpace(out.category) == "" {
		out = poolExhaustion{category: "configuration", message: poolNoAccountsMessage}
	}
	apperrors.New(out.category, out.message, apperrors.StatusForCategory(out.category)).WriteResponse(w)
}
