package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFlareSolverrSolveOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" {
			t.Fatalf("path = %q, want /v1", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload["cmd"] != "request.get" {
			t.Fatalf("cmd = %v", payload["cmd"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": "ok",
			"solution": {
				"userAgent": "Mozilla/5.0 (Macintosh) Chrome/148.0.0.0",
				"cookies": [
					{"name": "cf_clearance", "value": "abc123"},
					{"name": "__cf_bm", "value": "xyz789"},
					{"name": "other", "value": "ignored"}
				]
			}
		}`))
	}))
	defer server.Close()

	solver := flaresolverrSolver{}
	cfg := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: server.URL, TargetURL: "https://grok.com", Timeout: 30 * time.Second}
	solution, err := solver.Solve(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("solve failed: %v", err)
	}
	if !strings.Contains(solution.Cookies, "cf_clearance=abc123") {
		t.Fatalf("cookies = %q", solution.Cookies)
	}
	if !strings.Contains(solution.Cookies, "__cf_bm=xyz789") {
		t.Fatalf("cookies missing __cf_bm: %q", solution.Cookies)
	}
	if strings.Contains(solution.Cookies, "other=ignored") {
		t.Fatalf("non-cloudflare cookie should be stripped: %q", solution.Cookies)
	}
	if solution.UserAgent == "" {
		t.Fatal("expected user agent")
	}
}

func TestFlareSolverrSolveFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","message":"challenge failed"}`))
	}))
	defer server.Close()

	solver := flaresolverrSolver{}
	cfg := ClearanceConfig{Mode: "flaresolverr", FlareSolverrURL: server.URL, TargetURL: "https://grok.com"}
	if _, err := solver.Solve(context.Background(), cfg, ""); err == nil {
		t.Fatal("expected error on failed solve")
	}
}

func TestFlareSolverrSanitizeMessage(t *testing.T) {
	msg := "proxy http://user:pass@host:8080 and token=secret123"
	got := sanitizeFlareSolverrMessage(msg)
	if strings.Contains(got, "pass@") {
		t.Fatalf("proxy credentials not redacted: %q", got)
	}
	if strings.Contains(got, "secret123") {
		t.Fatalf("named credential not redacted: %q", got)
	}
}

func TestSanitizeCloudflareCookies(t *testing.T) {
	oversized := strings.Repeat("x", maxCloudflareCookieBytes+1)
	got := SanitizeCloudflareCookies("CF_CLEARANCE=a; __cf_bm=b; _cfuvid=device; cf_chl_2=challenge; cf_clearance=duplicate; foo=bar; cf_chl_bad=has\nnewline; __cf_bm=" + oversized)
	want := "cf_clearance=a; __cf_bm=b; _cfuvid=device; cf_chl_2=challenge"
	if got != want {
		t.Fatalf("got = %q want %q", got, want)
	}
}

func TestFlareSolverrEndpointNormalization(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8191":        "http://localhost:8191/v1",
		"http://localhost:8191/":       "http://localhost:8191/v1",
		"http://localhost:8191/v1":     "http://localhost:8191/v1",
		"http://localhost:8191/custom": "http://localhost:8191/custom/v1",
	}
	for in, want := range cases {
		got, err := flaresolverrEndpoint(in)
		if err != nil {
			t.Fatalf("endpoint %q: %v", in, err)
		}
		if got != want {
			t.Fatalf("endpoint %q = %q, want %q", in, got, want)
		}
	}
}

func TestFlareSolverrEndpointInvalid(t *testing.T) {
	for _, in := range []string{"", "ftp://x", "http://user@host:8191", "http://host:8191?q=1"} {
		if _, err := flaresolverrEndpoint(in); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}
