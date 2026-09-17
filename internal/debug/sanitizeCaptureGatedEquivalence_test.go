package debug

import (
	"strings"
	"testing"
)

// ungatedSanitize is the sweep sanitizeCapture used before the prefilters:
// every pass run unconditionally. It stays here as the reference the gated
// version must keep agreeing with, because a prefilter that misses a match does
// not fail loudly — it silently leaves a credential in a retained bundle.
func ungatedSanitize(text string) string {
	text = credentialPattern.ReplaceAllString(text, `${1}"[REDACTED]"`)
	text = bearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
	text = urlPasswordPattern.ReplaceAllString(text, "${1}${2}:[REDACTED]@")
	return opaquePattern.ReplaceAllString(text, "[REDACTED]")
}

func TestSanitizeCaptureMatchesUngatedSweep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no trigger at all", "the quick brown fox jumps over the lazy dog"},
		{"json authorization bearer", `{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig"}`},
		{"json api key", `{"api_key":"sk-abcdefghijklmnop","model":"grok-4"}`},
		{"bearer lowercase", "sent bearer abc.def-token to the host"},
		{"bearer uppercase", "sent BEARER ABC.DEF-TOKEN to the host"},
		{"bearer mixed case", "sent BeArEr abc.def-token to the host"},
		{"bearer tab separator", "authorization: Bearer\tabcdefghij"},
		{"bearer without token", "the Bearer of good news arrived"},
		{"url with password", "https://alice:hunter2@example.com/v1/messages"},
		{"url without password", "https://example.com/v1/messages"},
		{"bare scheme separator", "see :// for the separator"},
		{"opaque sk", "key sk-abcdefghijklmnop is live"},
		{"opaque eyJ", "token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 is live"},
		{"opaque sk too short", "key sk-short is not a token"},
		{"opaque eyJ too short", "token eyJabc is not a token"},
		{"sse fragment", "data: {\"delta\":\"Bearer aaa.bbb\",\"session\":\"s-1\"}\n\n"},
		{"mixed", `{"cookie":"sso=abc","note":"https://u:p@h/x and sk-1234567890ab"}`},
		{"prompt text only", "解释一下 TCP 三次握手，顺便谈谈拥塞控制"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := ungatedSanitize(tc.in)
			if got := sanitizeCapture(tc.in); got != want {
				t.Fatalf("gated sanitize diverged from the ungated sweep\n input: %q\n gated: %q\n  want: %q", tc.in, got, want)
			}
		})
	}
}

// The prefilters must not have turned redaction into a no-op: every credential
// spelling below has to be gone from the output.
func TestSanitizeCaptureStillRedactsCredentials(t *testing.T) {
	t.Parallel()

	secrets := []string{
		"eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"ABC.DEF-TOKEN",
		"abc.def-token",
		"sk-abcdefghijklmnop",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"alice:hunter2@",
	}
	inputs := []string{
		`{"authorization":"Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig"}`,
		"sent BEARER ABC.DEF-TOKEN to the host",
		"sent bearer abc.def-token to the host",
		"key sk-abcdefghijklmnop is live",
		"token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 is live",
		"https://alice:hunter2@example.com/v1/messages",
	}
	for _, in := range inputs {
		out := sanitizeCapture(in)
		for _, secret := range secrets {
			if strings.Contains(out, secret) {
				t.Fatalf("sanitizeCapture left %q in %q", secret, out)
			}
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Fatalf("sanitizeCapture redacted nothing in %q -> %q", in, out)
		}
	}
}
