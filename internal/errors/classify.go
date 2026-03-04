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

// ClassifyAccountStatus maps an error string to an HTTP status code string
// ("401", "403", "404", "429") or returns "" if the error does not indicate
// a recognisable account-level issue.
func ClassifyAccountStatus(errStr string) string {
	lower := strings.ToLower(errStr)
	// Model name/mapping errors should not poison account status.
	if strings.Contains(lower, "model is not found") || strings.Contains(lower, "model not found") {
		return ""
	}
	switch {
	case HasExplicitHTTPStatus(lower, "401") || strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out") || strings.Contains(lower, "unauthorized"):
		return "401"
	case HasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case HasExplicitHTTPStatus(lower, "404"):
		return "404"
	case HasExplicitHTTPStatus(lower, "429") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits"):
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
		return UpstreamErrorClass{Category: "canceled", Retryable: false, SwitchAccount: false}
	case HasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out"):
		return UpstreamErrorClass{Category: "auth", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "403"):
		return UpstreamErrorClass{Category: "auth_blocked", Retryable: true, SwitchAccount: true}
	case HasExplicitHTTPStatus(lower, "404"):
		return UpstreamErrorClass{Category: "auth_blocked", Retryable: false, SwitchAccount: false}
	case strings.Contains(lower, "input is too long") || HasExplicitHTTPStatus(lower, "400"):
		return UpstreamErrorClass{Category: "client", Retryable: false, SwitchAccount: false}
	case HasExplicitHTTPStatus(lower, "429") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "no remaining quota") ||
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
	case HasExplicitHTTPStatus(lower, "500") || HasExplicitHTTPStatus(lower, "502") || HasExplicitHTTPStatus(lower, "503") || HasExplicitHTTPStatus(lower, "504"):
		return UpstreamErrorClass{Category: "server", Retryable: true, SwitchAccount: true}
	default:
		return UpstreamErrorClass{Category: "unknown", Retryable: true, SwitchAccount: true}
	}
}
