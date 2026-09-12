package errors

import "testing"

// TestClassifyAccountStatus_LeadingStatusCodePrefix pins the "<code>: <detail>"
// error shape that provider layers produce (e.g. the Grok wrapper
// "401: grok session unauthenticated"). Before this was recognised a rejected
// credential classified as "" and was persisted as a healthy account.
func TestClassifyAccountStatus_LeadingStatusCodePrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"grok wrapper 401", "401: grok session unauthenticated", "401"},
		{"grok nested wrapper", "failed to verify grok account: 401: grok session unauthenticated", "401"},
		{"403 prefix", "403: blocked", "403"},
		{"429 prefix", "429: rate limited", "429"},
		{"402 prefix", "402: out of credits", "402"},
		{"404 prefix", "404", "404"},
		{"colon no space", "401:signed out", "401"},
		{"not a prefix", "upstream returned 401 mid-message about a model", ""},
		{"longer number is not a code", "4040 widgets missing", ""},
		// The upstream session endpoint answers {"status":"unauthenticated"} with
		// HTTP 200. That body is a refused credential, so it must classify as 401
		// even without an HTTP status in the text.
		{"bare unauthenticated body", "grok session unauthenticated", "401"},
		{"unrelated detail", "the cache is cold", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAccountStatus(tc.in); got != tc.want {
				t.Fatalf("ClassifyAccountStatus(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestClassifyAccountStatus_LeadingPrefixStillYieldsToModelErrors keeps the
// existing guard: a model-name failure must never poison account status even if
// it is prefixed with a status code.
func TestClassifyAccountStatus_LeadingPrefixStillYieldsToModelErrors(t *testing.T) {
	if got := ClassifyAccountStatus("404: model not found"); got != "" {
		t.Fatalf("ClassifyAccountStatus(404: model not found) = %q, want empty", got)
	}
}
