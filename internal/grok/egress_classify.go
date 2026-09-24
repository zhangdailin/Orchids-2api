package grok

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// 403/429 classification helpers, ported from grok2api account_block.go plus
// Cloudflare challenge detection. The unified entry point ClassifyUpstreamResponse
// is response-aware (status + headers + body) so header-only signals such as
// CF-Mitigated are not lost. Precedence:
//
//	Cloudflare challenge > explicit account block > 429 > generic forbidden
//
// A generic 403 is deliberately NOT treated as an account block; only explicit
// "blocked-user"/"user is blocked" language marks the account.

// UpstreamErrorKind categorizes an upstream failure for account/egress handling.
type UpstreamErrorKind int

const (
	UpstreamErrorUnknown UpstreamErrorKind = iota
	UpstreamErrorAccountBlock
	UpstreamErrorCloudflareChallenge
	UpstreamErrorGenericForbidden
	UpstreamErrorRateLimited
)

func (k UpstreamErrorKind) String() string {
	switch k {
	case UpstreamErrorAccountBlock:
		return "account_block"
	case UpstreamErrorCloudflareChallenge:
		return "cloudflare_challenge"
	case UpstreamErrorGenericForbidden:
		return "generic_forbidden"
	case UpstreamErrorRateLimited:
		return "rate_limited"
	default:
		return "unknown"
	}
}

// ClassifyUpstreamResponse classifies an upstream HTTP response.
func ClassifyUpstreamResponse(status int, header http.Header, body []byte) UpstreamErrorKind {
	if status == http.StatusTooManyRequests {
		return UpstreamErrorRateLimited
	}
	if header != nil {
		if hasCloudflareChallengeHeader(header) {
			return UpstreamErrorCloudflareChallenge
		}
	}
	switch status {
	case http.StatusForbidden:
		if IsCloudflareChallengeBody(body) {
			return UpstreamErrorCloudflareChallenge
		}
		if IsDefinitiveAccountBlockBody(body) {
			return UpstreamErrorAccountBlock
		}
		return UpstreamErrorGenericForbidden
	case http.StatusUnauthorized:
	}
	return UpstreamErrorUnknown
}

// ClassifyUpstreamError classifies a returned error, preferring the typed
// grokUpstreamError and falling back to the legacy "grok upstream status=.. body=.."
// text format for plain errors.
func ClassifyUpstreamError(err error) UpstreamErrorKind {
	if err == nil {
		return UpstreamErrorUnknown
	}
	var typed *grokUpstreamError
	if errors.As(err, &typed) {
		return ClassifyUpstreamResponse(typed.status, typed.header, []byte(typed.body))
	}
	status := parseUpstreamStatus(err)
	return ClassifyUpstreamResponse(status, nil, []byte(upstreamErrorBody(err)))
}

// hasCloudflareChallengeHeader detects header-only Cloudflare challenge signals.
func hasCloudflareChallengeHeader(header http.Header) bool {
	if value := strings.TrimSpace(header.Get("CF-Mitigated")); value != "" {
		return true
	}
	for name := range header {
		if strings.Contains(strings.ToLower(name), "cf-mitigated") {
			return true
		}
	}
	return false
}

// IsDefinitiveAccountBlockBody accepts only explicit error code or message
// signals that a Grok account is blocked/suspended.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDefinitiveAccountBlockText(string(body))
	}
	values := collectJSONStrings(payload)
	return IsDefinitiveAccountBlockText(strings.Join(values, " "))
}

// IsDefinitiveAccountBlockText matches explicit account-block language.
func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

// IsCloudflareChallengeBody detects a Cloudflare interstitial/challenge page.
// These are egress/clearance failures, not account failures.
func IsCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"cf-mitigated",
		"cf_chl_opt",
		"__cf_chl",
		"cf-chl",
		"challenge-platform",
		"turnstile",
		"just a moment",
		"enable javascript and cookies to continue",
		"verify you are human",
		"attention required",
		"request rejected by anti-bot rules",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// collectJSONStrings recursively gathers string values from a decoded JSON tree
// (objects and arrays) so nested error/message fields are inspected, not just
// the shallow top level.
func collectJSONStrings(value any) []string {
	var out []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			out = append(out, typed)
		}
	}
	walk(value)
	return out
}
