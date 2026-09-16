package errors

import "strings"

// PublicMessage converts an internal/upstream error into a stable, actionable
// message without exposing response bodies, credentials, cookies or provider
// implementation details. The request ID is returned separately by middleware.
func PublicMessage(errText string) string {
	return messageForCategory(ClassifyUpstreamError(errText).Category)
}

// PublicMessageForCategory is PublicMessage for a caller that already classified
// the error. It exists so a client-visible message and the classification that
// produced it cannot drift apart — and so a response rebuilt after redaction
// reports the same text the redaction wrote.
func PublicMessageForCategory(category string) string {
	return messageForCategory(category)
}

// messageForCategory is the single source of truth for the client-visible text
// of each category. An empty category is the caller that had no error text to
// classify, and keeps its shorter wording.
func messageForCategory(category string) string {
	switch category {
	case "configuration":
		return "This provider is not configured correctly. Contact the gateway administrator."
	case "auth":
		return "The upstream account session has expired. Re-authenticate the account and retry."
	case "auth_blocked":
		return "The upstream account is not allowed to use this feature. Check its plan and permissions."
	case "quota_exhausted":
		return "The available upstream accounts have exhausted their quota. Retry after the quota resets or add capacity."
	case "rate_limit":
		return "The available upstream accounts are rate-limited. Retry after the cooldown."
	case "model_unavailable":
		return "The requested model is unavailable for the selected upstream accounts."
	case "client":
		return "The upstream rejected the request parameters or model. Check the request and model selection."
	case "timeout":
		return "The upstream stream timed out while waiting for generated output."
	case "network":
		return "The gateway lost its connection to the upstream service. Retry later."
	case "server":
		return "The upstream service is temporarily unavailable. Retry later."
	case "protocol":
		return "The upstream returned an unsupported stream format. Use the request ID to inspect diagnostics."
	case "local_overload":
		return "The gateway is temporarily overloaded. Retry later."
	case "canceled":
		return "The request was canceled."
	default:
		if strings.TrimSpace(category) == "" {
			return "The upstream request failed."
		}
		return "The upstream request failed. Use the request ID to inspect diagnostics."
	}
}
