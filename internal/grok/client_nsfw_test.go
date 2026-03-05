package grok

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParseUpstreamStatus(t *testing.T) {
	if got := parseUpstreamStatus(fmt.Errorf("grok upstream status=403 body=forbidden")); got != 403 {
		t.Fatalf("parseUpstreamStatus()=%d want=403", got)
	}
	if got := parseUpstreamStatus(fmt.Errorf("grok upstream request failed")); got != 0 {
		t.Fatalf("parseUpstreamStatus()=%d want=0", got)
	}
	if got := parseUpstreamStatus(fmt.Errorf("grok upstream status=abc body=bad")); got != 0 {
		t.Fatalf("parseUpstreamStatus()=%d want=0", got)
	}
}

func TestEnableNSFWDetailed_EmptyToken(t *testing.T) {
	c := New(nil)
	res := c.EnableNSFWDetailed(context.Background(), "   ")
	if res.Success {
		t.Fatalf("EnableNSFWDetailed should fail on empty token")
	}
	if res.Error == "" {
		t.Fatalf("EnableNSFWDetailed should return error message")
	}
}

func TestHeaders_NormalizeSSOToken(t *testing.T) {
	c := New(nil)
	h := c.headers("sso=token-abc; Path=/; HttpOnly")
	cookie := h.Get("Cookie")
	if !strings.Contains(cookie, "sso=token-abc; sso-rw=token-abc") {
		t.Fatalf("unexpected cookie=%q", cookie)
	}
	if strings.Contains(cookie, "sso=sso=") {
		t.Fatalf("token should be normalized, cookie=%q", cookie)
	}
}
