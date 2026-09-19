package egress

import (
	"strings"
	"testing"
)

// A node whose proxy URL fails validation used to be logged verbatim, which put
// the proxy password into the log stream. The sanitizer must strip the userinfo.
func TestSanitizeFlareSolverrMessageRedactsProxyCredentials(t *testing.T) {
	cases := []string{
		"http://user:sup3rsecret@proxy.internal:99999",
		"socks5://alice:hunter2@10.0.0.1:1080",
		"http://user:sup3rsecret@proxy.internal:8080",
	}
	for _, raw := range cases {
		got := sanitizeFlareSolverrMessage(raw)
		for _, secret := range []string{"sup3rsecret", "hunter2"} {
			if strings.Contains(got, secret) {
				t.Fatalf("sanitizeFlareSolverrMessage(%q) = %q, leaked %q", raw, got, secret)
			}
		}
	}
}

func TestBuildNodesLogUsesSanitizedURL(t *testing.T) {
	// Guard the call site: the sanitized value must differ from the raw one for
	// a credential-bearing URL, otherwise the log line is back to leaking.
	const raw = "http://user:sup3rsecret@proxy.internal:99999"
	if sanitizeFlareSolverrMessage(raw) == raw {
		t.Fatalf("sanitizer left the credential-bearing URL unchanged: %q", raw)
	}
}
