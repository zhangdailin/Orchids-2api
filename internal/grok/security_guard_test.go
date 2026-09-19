package grok

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestValidateRemoteFetchURL(t *testing.T) {
	for _, raw := range []string{
		"http://user:pass@example.com/a.png",
		"file:///etc/passwd",
		"ftp://example.com/a.png",
		"http:///a.png",
		"",
	} {
		if _, err := validateRemoteFetchURL(raw); err == nil {
			t.Fatalf("validateRemoteFetchURL(%q) should fail", raw)
		}
	}
	if _, err := validateRemoteFetchURL("https://example.com/a.png"); err != nil {
		t.Fatalf("validateRemoteFetchURL(valid) = %v", err)
	}
}

func TestIsPublicAddrRejectsInternalRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.0.0.5", "172.16.4.4", "192.168.1.1",
		"169.254.169.254", "0.0.0.0", "100.64.0.1", "192.0.0.1",
		"198.18.0.1", "224.0.0.1", "240.0.0.1", "fc00::1", "fe80::1",
	}
	for _, raw := range blocked {
		if isPublicAddr(netip.MustParseAddr(raw)) {
			t.Fatalf("isPublicAddr(%s) = true, want false", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicAddr(netip.MustParseAddr(raw)) {
			t.Fatalf("isPublicAddr(%s) = false, want true", raw)
		}
	}
}

func TestResolvePublicAddrsBlocksInternalHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "[::1]", "10.1.2.3"} {
		if _, err := resolvePublicAddrs(context.Background(), strings.Trim(host, "[]")); err == nil {
			t.Fatalf("resolvePublicAddrs(%s) should fail", host)
		}
	}
}

// The gateway used to treat any client-supplied image_url as a fetch target.
// These are the payloads the guard has to stop before a dial happens.
func TestFetchRemoteAsDataURIBlocksSSRFTargets(t *testing.T) {
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:8080/admin",
		"http://[::1]:9000/internal",
		"http://10.0.0.1/secret.png",
		"http://user:pass@8.8.8.8/x.png",
	} {
		_, err := fetchRemoteAsDataURI(raw, 0, nil)
		if err == nil {
			t.Fatalf("fetchRemoteAsDataURI(%q) should be refused", raw)
		}
		if !errors.Is(err, errRemoteFetchBlocked) {
			t.Fatalf("fetchRemoteAsDataURI(%q) error = %v, want errRemoteFetchBlocked", raw, err)
		}
	}
}

func TestFetchRemoteAsDataURIFollowsPublicTarget(t *testing.T) {
	// A local httptest server is a loopback target, so the guard must refuse it
	// even though the server is reachable — that is the property under test.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("png"))
	}))
	defer server.Close()
	if _, err := fetchRemoteAsDataURI(server.URL, 0, nil); !errors.Is(err, errRemoteFetchBlocked) {
		t.Fatalf("loopback fetch error = %v, want errRemoteFetchBlocked", err)
	}
}

func TestSanitizeBaseHost(t *testing.T) {
	valid := map[string]string{
		"api.example.com":      "api.example.com",
		"api.example.com:8443": "api.example.com:8443",
		"127.0.0.1:8080":       "127.0.0.1:8080",
		"localhost":            "localhost",
	}
	for in, want := range valid {
		if got := sanitizeBaseHost(in); got != want {
			t.Fatalf("sanitizeBaseHost(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"evil.example/path", "evil.example?x=1", "user@evil.example",
		"bad_host", "evil.example#frag", "",
	} {
		if got := sanitizeBaseHost(in); got != "" {
			t.Fatalf("sanitizeBaseHost(%q) = %q, want empty", in, got)
		}
	}
}

func TestDetectPublicBaseURLSanitizesForwardedHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://gateway.local/v1/chat/completions", nil)
	req.Host = "gateway.local"
	req.Header.Set("X-Forwarded-Host", "evil.example/steal")
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := detectPublicBaseURL(req); got != "https://gateway.local" {
		t.Fatalf("detectPublicBaseURL() = %q, want https://gateway.local", got)
	}
	req.Header.Set("X-Forwarded-Host", "cdn.example:8443")
	if got := detectPublicBaseURL(req); got != "https://cdn.example:8443" {
		t.Fatalf("detectPublicBaseURL() = %q, want https://cdn.example:8443", got)
	}
	req.Header.Set("X-Forwarded-Proto", "javascript")
	if got := detectPublicBaseURL(req); !strings.HasPrefix(got, "http") || strings.HasPrefix(got, "javascript") {
		t.Fatalf("detectPublicBaseURL() = %q, want http scheme fallback", got)
	}
}

// Session credentials are bound to the API origin; CDN hosts must stay anonymous.
func TestAssetDownloadSkipsCredentialsForCDNHosts(t *testing.T) {
	c := New(nil)
	for _, host := range []string{
		"https://vidgen.x.ai/abc.mp4",
		"https://imagine-public.x.ai/a.png",
		"https://imgen.x.ai/a.png",
		"https://eu.vidgen.x.ai/abc.mp4",
	} {
		headers := c.assetDownloadHeaders("sso=token", host)
		if headers.Get("Cookie") != "" {
			t.Fatalf("assetDownloadHeaders(%s) leaked Cookie=%q", host, headers.Get("Cookie"))
		}
	}
	headers := c.assetDownloadHeaders("sso=token", "https://grok.com/users/x/generated/y/image.png")
	if headers.Get("Cookie") == "" {
		t.Fatalf("grok.com asset download lost its session cookie")
	}
}

func TestImagineNSFWAllowedRequiresOperatorConsent(t *testing.T) {
	enabled, disabled := true, false
	requested := true
	cases := []struct {
		name string
		cfg  *config.Config
		req  *bool
		want bool
	}{
		{"server off, client asks", &config.Config{ImageNSFW: &disabled}, &requested, false},
		{"server on, client asks", &config.Config{ImageNSFW: &enabled}, &requested, true},
		{"server on, client silent", &config.Config{ImageNSFW: &enabled}, nil, false},
		{"server on, client declines", &config.Config{ImageNSFW: &enabled}, &disabled, false},
		{"no config", nil, &requested, false},
	}
	for _, tc := range cases {
		h := &Handler{cfg: tc.cfg}
		if got := h.imagineNSFWAllowed(tc.req); got != tc.want {
			t.Fatalf("%s: imagineNSFWAllowed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The egress-node log redaction helper lives in package egress and is covered
// by egress/redact_test.go.
