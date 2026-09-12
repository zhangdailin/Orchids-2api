package errors

import "strings"

// HasExplicitHTTPStatus checks whether an error string contains an explicit
// reference to the given HTTP status code (e.g. "HTTP 401", "status=429").
func HasExplicitHTTPStatus(lower string, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || lower == "" {
		return false
	}
	patterns := []string{
		"http " + code,
		"http/1.1 " + code,
		"http/2 " + code,
		"status " + code,
		"status=" + code,
		"status:" + code,
		"statuscode " + code,
		"statuscode=" + code,
		"status code " + code,
		"code " + code,
		"code=" + code,
		"code:" + code,
		"response status " + code,
		"response code " + code,
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// statusCodePrefixes lists the account-level codes that callers sometimes encode
// as a bare "<code>" delimiter token on an error message.
var statusCodePrefixes = []string{"401", "402", "403", "404", "429"}

// codeAtDelimiter returns the status code starting at offset start of the
// already-lowercased string, when the code is followed by a delimiter (":"),
// whitespace, or the end of the string. The delimiter requirement keeps longer
// numbers ("4040 widgets") from matching.
func codeAtDelimiter(lower string, start int) (string, bool) {
	if start < 0 || start >= len(lower) {
		return "", false
	}
	for _, code := range statusCodePrefixes {
		if !strings.HasPrefix(lower[start:], code) {
			continue
		}
		rest := lower[start+len(code):]
		if rest == "" || strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, " ") {
			return code, true
		}
	}
	return "", false
}

// scanStatusCode finds a delimiter-bounded status code in the already-lowercased
// string, preferring the leftmost occurrence. Provider layers concatenate the
// upstream status into the error text, so either the leading "<code>: <detail>"
// form or the wrapped "context: <code>: <detail>" form must stay recognised.
func scanStatusCode(lower string) string {
	search := lower
	offset := 0
	for {
		index := strings.Index(search, ":")
		if index < 0 {
			return ""
		}
		position := offset + index + 1
		trimmed := position
		for trimmed < len(lower) && lower[trimmed] == ' ' {
			trimmed++
		}
		if code, ok := codeAtDelimiter(lower, trimmed); ok {
			return code
		}
		offset = position
		search = lower[offset:]
	}
}

// LeadingStatusCode returns the status code encoded in a leading "<code>:<detail>"
// prefix (also "HTTP 401", "status=401" is handled by HasExplicitHTTPStatus), or
// "" when the string carries no such prefix. A bare leading code such as "401"
// counts as well.
func LeadingStatusCode(errStr string) string {
	trimmed := strings.TrimSpace(strings.ToLower(errStr))
	code, ok := codeAtDelimiter(trimmed, 0)
	if !ok {
		return ""
	}
	return code
}

// ClassifyAccountStatus maps an error string to an HTTP status code string
// ("401", "403", "404", "429") or returns "" if the error does not indicate
// a recognisable account-level issue.
func ClassifyAccountStatus(errStr string) string {
	lower := strings.ToLower(errStr)
	// Model name/mapping errors should not poison account status.
	if strings.Contains(lower, "model is not found") || strings.Contains(lower, "model not found") {
		return ""
	}
	// A status reason persisted by an admin handler is the wrapped error string,
	// so recognise the code that sits inside the wrap chain as well.
	if code := LeadingStatusCode(lower); code != "" {
		return code
	}
	if code := scanStatusCode(lower); code != "" {
		return code
	}
	switch {
	case HasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "signed out") ||
		strings.Contains(lower, "signed_out") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "no active sessions found"):
		return "401"
	case HasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case HasExplicitHTTPStatus(lower, "404"):
		return "404"
	case HasExplicitHTTPStatus(lower, "402") ||
		strings.Contains(lower, "insufficient_funds") ||
		strings.Contains(lower, "insufficient funding") ||
		strings.Contains(lower, "available funding is insufficient") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits") ||
		strings.Contains(lower, "quota_limit"):
		return "402"
	case
		HasExplicitHTTPStatus(lower, "429") ||
			strings.Contains(lower, "too many requests") ||
			strings.Contains(lower, "rate limit") ||
			strings.Contains(lower, "rate_limit") ||
			strings.Contains(lower, "no remaining quota") ||
			strings.Contains(lower, "quota exceeded"):
		return "429"
	default:
		return ""
	}
}

// UpstreamErrorClass describes the category and retry semantics of an upstream error.
type UpstreamErrorClass struct {
	Category      string
	Retryable     bool
	SwitchAccount bool
}

// ClassifyUpstreamError categorises an upstream error string into a structured
// class that drives retry and account-switching decisions.
func ClassifyUpstreamError(errStr string) UpstreamErrorClass {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "context canceled") || strings.Contains(lower, "canceled"):
		return UpstreamErrorClass{Category: "canceled"}
	case strings.Contains(lower, "model is not found") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "no_implementation_available") ||
		strings.Contains(lower, "context_window_exceeded") ||
		strings.Contains(lower, "max_token_limit"):
		return UpstreamErrorClass{Category: "client"}
	case HasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "signed out") ||
		strings.Contains(lower, "signed_out") ||
		strings.Contains(lower, "invalid_api_key"):
		return UpstreamErrorClass{Category: "auth", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "403"):
		return UpstreamErrorClass{Category: "auth_blocked", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "404"):
		return UpstreamErrorClass{Category: "auth_blocked"}
	case isWarpModelUnavailableError(lower):
		return UpstreamErrorClass{Category: "model_unavailable", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "input is too long") || HasExplicitHTTPStatus(lower, "400"):
		return UpstreamErrorClass{Category: "client"}
	case HasExplicitHTTPStatus(lower, "429") ||
		HasExplicitHTTPStatus(lower, "402") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "rate_limit") ||
		strings.Contains(lower, "insufficient_funds") ||
		strings.Contains(lower, "insufficient funding") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "quota_limit") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits"):
		return UpstreamErrorClass{Category: "rate_limit", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "context deadline"):
		return UpstreamErrorClass{Category: "timeout", Retryable: true, SwitchAccount: true}
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "use of closed") ||
		strings.Contains(lower, "broken pipe") || strings.HasSuffix(lower, ": eof") || lower == "eof":
		return UpstreamErrorClass{Category: "network", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "500") || HasExplicitHTTPStatus(lower, "502") || HasExplicitHTTPStatus(lower, "503") || HasExplicitHTTPStatus(lower, "504") ||
		strings.Contains(lower, "llm_unavailable") ||
		strings.Contains(lower, "internal_error"):
		return UpstreamErrorClass{Category: "server", Retryable: true, SwitchAccount: true}
	default:
		return UpstreamErrorClass{Category: "unknown", Retryable: true, SwitchAccount: true}
	}
}

func isWarpModelUnavailableError(lower string) bool {
	if !strings.Contains(lower, "warp") {
		return false
	}
	return strings.Contains(lower, "requested base model") &&
		(strings.Contains(lower, "not allowed") || strings.Contains(lower, "no model available")) ||
		strings.Contains(lower, "llm_unavailable") || strings.Contains(lower, "model unavailable")
}
