package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyMiddlewareRejectsSpoofedForwardingHeaders(t *testing.T) {
	middleware, err := TrustedProxyMiddleware([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientIP(r); got != "203.0.113.10" {
			t.Fatalf("client IP = %q", got)
		}
		if got := r.Header.Get("X-Forwarded-For"); got != "" {
			t.Fatalf("untrusted forwarding header survived: %q", got)
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "" {
			t.Fatalf("untrusted proto survived: %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTrustedProxyMiddlewareWalksForwardedChain(t *testing.T) {
	middleware, err := TrustedProxyMiddleware([]string{"10.0.0.0/8", "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ClientIP(r); got != "198.51.100.9" {
			t.Fatalf("client IP = %q", got)
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "https" {
			t.Fatalf("trusted proto = %q", got)
		}
		if got := r.Header.Get("X-Forwarded-For"); got != "198.51.100.9" {
			t.Fatalf("sanitized forwarding chain = %q", got)
		}
		if got := r.Header.Get("X-Forwarded-Host"); got != "api.example.com" {
			t.Fatalf("sanitized forwarded host = %q", got)
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 192.0.2.10")
	req.Header.Set("X-Forwarded-Proto", "javascript, https")
	req.Header.Set("X-Forwarded-Host", "evil.example, api.example.com")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTrustedProxyMiddlewareRejectsInvalidNetwork(t *testing.T) {
	if _, err := TrustedProxyMiddleware([]string{"0.0.0.0/0"}); err != nil {
		// A broad network is syntactically valid and deliberately explicit.
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := TrustedProxyMiddleware([]string{"not-an-ip"}); err == nil {
		t.Fatal("expected invalid proxy error")
	}
}

// An anonymous allowlist lets the named sources through without a key and nobody
// else; an empty list is the reference behaviour (everyone needs a key).
func TestAnonymousAllowlistAllowsOnlyNamedSources(t *testing.T) {
	empty, err := NewAnonymousAllowlist(nil)
	if err != nil {
		t.Fatalf("NewAnonymousAllowlist(nil): %v", err)
	}
	if !empty.Empty() {
		t.Fatal("a nil list is not empty")
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	request.RemoteAddr = "203.0.113.20:4444"
	if empty.Allows(request) {
		t.Fatal("an empty allowlist allowed a caller")
	}

	list, err := NewAnonymousAllowlist([]string{"203.0.113.20", "198.51.100.0/24", "2001:db8::1"})
	if err != nil {
		t.Fatalf("NewAnonymousAllowlist: %v", err)
	}
	cases := []struct {
		remote string
		want   bool
	}{
		{"203.0.113.20:1111", true},
		{"203.0.113.21:1111", false},
		{"198.51.100.40:1111", true},
		{"198.51.101.40:1111", false},
		{"[2001:db8::1]:1111", true},
		{"[2001:db8::2]:1111", false},
		{"not-an-address", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
		req.RemoteAddr = tc.remote
		if got := list.Allows(req); got != tc.want {
			t.Fatalf("Allows(%q)=%v want %v", tc.remote, got, tc.want)
		}
	}

	// A malformed entry is an error, so a typo cannot silently widen the list.
	if _, err := NewAnonymousAllowlist([]string{"10.0.0.0/8", "nonsense"}); err == nil {
		t.Fatal("a malformed entry was accepted")
	}
}

// A forwarded client address is only honoured when the peer is a trusted proxy,
// which is what the allowlist must decide on.
func TestAnonymousAllowlistUsesTheTrustedClientAddress(t *testing.T) {
	list, err := NewAnonymousAllowlist([]string{"203.0.113.20"})
	if err != nil {
		t.Fatalf("NewAnonymousAllowlist: %v", err)
	}
	wrap, err := TrustedProxyMiddleware([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("TrustedProxyMiddleware: %v", err)
	}
	var allowed bool
	handler := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed = list.Allows(r)
		w.WriteHeader(http.StatusOK)
	}))

	// A trusted peer's forwarded address is the client.
	trusted := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	trusted.RemoteAddr = "127.0.0.1:5555"
	trusted.Header.Set("X-Forwarded-For", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), trusted)
	if !allowed {
		t.Fatal("a trusted proxy's forwarded client address was not honoured")
	}

	// The same header from an untrusted peer is cleared, so the peer itself decides.
	untrusted := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	untrusted.RemoteAddr = "203.0.113.9:5555"
	untrusted.Header.Set("X-Forwarded-For", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), untrusted)
	if allowed {
		t.Fatal("an untrusted peer spoofed its way onto the allowlist")
	}
}

// Behind Cloudflare the original client is named by CF-Connecting-IP, and only a
// trusted peer's value counts: a direct caller cannot name itself.
func TestTrustedProxyPrefersCloudflareClientHeader(t *testing.T) {
	wrap, err := TrustedProxyMiddleware([]string{"127.0.0.1/32", "173.245.48.0/20"})
	if err != nil {
		t.Fatalf("TrustedProxyMiddleware: %v", err)
	}
	var resolved string
	handler := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved = ClientIP(r)
		w.WriteHeader(http.StatusOK)
	}))

	// Caddy on loopback forwards Cloudflare's client header, while the forwarded
	// chain only carries edge addresses.
	request := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	request.Header.Set("CF-Connecting-IP", "203.0.113.20")
	request.Header.Set("X-Forwarded-For", "173.245.48.9")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if resolved != "203.0.113.20" {
		t.Fatalf("resolved client=%q want the Cloudflare client address", resolved)
	}

	// A request from an untrusted peer has the header stripped, so the peer is the
	// client and cannot claim to be someone else.
	direct := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	direct.RemoteAddr = "203.0.113.9:5555"
	direct.Header.Set("CF-Connecting-IP", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), direct)
	if resolved != "203.0.113.9" {
		t.Fatalf("resolved client=%q want the real peer", resolved)
	}

	// With Cloudflare's ranges trusted, the forwarded chain also resolves to the
	// original client past the edge.
	chain := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	chain.RemoteAddr = "127.0.0.1:5555"
	chain.Header.Set("X-Forwarded-For", "203.0.113.20, 173.245.48.9")
	handler.ServeHTTP(httptest.NewRecorder(), chain)
	if resolved != "203.0.113.20" {
		t.Fatalf("resolved client=%q want the original client from the chain", resolved)
	}
}
