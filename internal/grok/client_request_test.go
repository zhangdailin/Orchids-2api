package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestCloneHeaderShallow_SetIsolation(t *testing.T) {
	src := http.Header{
		"X-Test":  []string{"alpha"},
		"X-Other": []string{"one"},
	}
	cloned := cloneHeaderShallow(src, 1)
	cloned.Set("X-Test", "beta")
	cloned.Set("X-New", "two")

	if got := src.Get("X-Test"); got != "alpha" {
		t.Fatalf("source header changed, got=%q", got)
	}
	if got := src.Get("X-New"); got != "" {
		t.Fatalf("source unexpectedly contains new key, got=%q", got)
	}
}

func TestBuildGrokCookie(t *testing.T) {
	got := buildGrokCookie("tok", "cf-clear", "bm")
	want := "sso=tok; sso-rw=tok; cf_clearance=cf-clear; __cf_bm=bm"
	if got != want {
		t.Fatalf("buildGrokCookie()=%q want=%q", got, want)
	}
}

func TestBuildGrokCookie_PreservesAppChatCookieFields(t *testing.T) {
	got := buildGrokCookie("foo=1; sso=tok; sso-rw=old; x-userid=user-1; Path=/; HttpOnly; __cf_bm=old-bm", "cf-clear", "bm")
	for _, want := range []string{
		"sso=tok",
		"sso-rw=tok",
		"foo=1",
		"x-userid=user-1",
		"cf_clearance=cf-clear",
		"__cf_bm=bm",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cookie=%q missing %q", got, want)
		}
	}
	for _, notWant := range []string{"Path=/", "HttpOnly", "sso-rw=old", "__cf_bm=old-bm"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("cookie=%q should not contain %q", got, notWant)
		}
	}
}

func TestAppChatHeaders_MatchBrowserProfile(t *testing.T) {
	c := New(nil)
	headers := c.appChatHeaders("plain-token")

	if got := headers.Get("Accept"); got != "*/*" {
		t.Fatalf("Accept=%q", got)
	}
	if got := headers.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
		t.Fatalf("Accept-Encoding=%q", got)
	}
	if got := headers.Get("Accept-Language"); got != "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7" {
		t.Fatalf("Accept-Language=%q", got)
	}
	if got := headers.Get("User-Agent"); got != defaultAppChatUA {
		t.Fatalf("User-Agent=%q", got)
	}
	if got := headers.Get("Sec-Ch-Ua"); got != defaultAppChatSecCHUA {
		t.Fatalf("Sec-Ch-Ua=%q", got)
	}
	if got := headers.Get("Sec-Ch-Ua-Platform"); got != `"Windows"` {
		t.Fatalf("Sec-Ch-Ua-Platform=%q", got)
	}
	if got := headers.Get("x-statsig-id"); got != defaultAppChatStatsigID {
		t.Fatalf("x-statsig-id=%q", got)
	}
	if got := headers.Get("Cookie"); got != "sso=plain-token; sso-rw=plain-token" {
		t.Fatalf("Cookie=%q", got)
	}
}

func TestGrokHeaders_UseConfiguredStatsigAndCloudflareCookies(t *testing.T) {
	c := New(&config.Config{
		GrokStatsigID:         "browser-statsig-id",
		GrokConfigCFClearance: "cf-clear",
		GrokConfigCFBM:        "bm-token",
	})

	for name, headers := range map[string]http.Header{
		"default": c.headers("plain-token"),
		"appChat": c.appChatHeaders("plain-token"),
		"console": c.consoleHeaders("plain-token"),
	} {
		if got := headers.Get("x-statsig-id"); got != "browser-statsig-id" {
			t.Fatalf("%s x-statsig-id=%q want browser-statsig-id", name, got)
		}
		cookie := headers.Get("Cookie")
		for _, want := range []string{"sso=plain-token", "sso-rw=plain-token", "cf_clearance=cf-clear", "__cf_bm=bm-token"} {
			if !strings.Contains(cookie, want) {
				t.Fatalf("%s Cookie=%q missing %q", name, cookie, want)
			}
		}
	}
}

func TestDoRequest_DoesNotMutateInputHeaders(t *testing.T) {
	t.Parallel()

	attempt := 0
	requestIDs := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		requestIDs = append(requestIDs, strings.TrimSpace(r.Header.Get("x-xai-request-id")))
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "retry", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(&config.Config{
		MaxRetries: 1,
		RetryDelay: 1,
	})
	headers := c.headers("token-abc")
	originalID := headers.Get("x-xai-request-id")

	resp, err := c.doRequest(context.Background(), srv.URL, http.MethodGet, nil, headers, http.StatusOK, false)
	if err != nil {
		t.Fatalf("doRequest() error: %v", err)
	}
	_ = resp.Body.Close()

	if got := headers.Get("x-xai-request-id"); got != originalID {
		t.Fatalf("input headers mutated: got=%q want=%q", got, originalID)
	}
	if attempt != 2 {
		t.Fatalf("expected 2 attempts, got=%d", attempt)
	}
	for i, id := range requestIDs {
		if id == "" {
			t.Fatalf("request %d missing x-xai-request-id", i+1)
		}
	}
}

func TestDoRequest_DoesNotFallbackForGenericTransportError(t *testing.T) {
	t.Parallel()

	primaryCalls := 0
	c := &Client{
		cfg: &config.Config{MaxRetries: 0},
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				primaryCalls++
				return nil, fmt.Errorf("dial tcp: connection refused")
			}),
		},
	}

	_, err := c.doRequest(context.Background(), "https://grok.com/rest/rate-limits", http.MethodPost, []byte(`{"message":"hi"}`), http.Header{}, http.StatusOK, false)
	if err == nil {
		t.Fatal("expected doRequest() to fail")
	}
	if primaryCalls != 1 {
		t.Fatalf("primaryCalls=%d want 1", primaryCalls)
	}
}

func TestAssetDownloadHeaders_DoesNotSendAuthToExternalURL(t *testing.T) {
	t.Parallel()

	c := New(nil)
	headers := c.assetDownloadHeaders("secret-token", "https://example.com/image.png")

	if got := headers.Get("Cookie"); got != "" {
		t.Fatalf("external asset Cookie=%q want empty", got)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("external asset Authorization=%q want empty", got)
	}
	if got := headers.Get("Accept"); !strings.Contains(got, "image/") {
		t.Fatalf("external asset Accept=%q want media accept header", got)
	}
}

func TestAssetDownloadHeaders_SendsAuthToGrokAssetURL(t *testing.T) {
	t.Parallel()

	c := New(nil)
	headers := c.assetDownloadHeaders("secret-token", "https://assets.grok.com/users/u/generated/a/image.png")

	if got := headers.Get("Cookie"); !strings.Contains(got, "sso=secret-token") {
		t.Fatalf("grok asset Cookie=%q want sso token", got)
	}
}

func TestDownloadAsset_DoesNotSendGrokHeadersToExternalURL(t *testing.T) {
	t.Parallel()

	var gotCookie, gotAuth, gotRequestID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		gotRequestID = r.Header.Get("x-xai-request-id")
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{1, 2, 3, 4})
	}))
	defer srv.Close()

	c := New(nil)
	data, mime, err := c.downloadAsset(context.Background(), "secret-token", srv.URL+"/image.png")
	if err != nil {
		t.Fatalf("downloadAsset() error: %v", err)
	}
	if len(data) == 0 || mime != "image/png" {
		t.Fatalf("downloadAsset() data=%d mime=%q", len(data), mime)
	}
	if gotCookie != "" {
		t.Fatalf("Cookie=%q want empty", gotCookie)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization=%q want empty", gotAuth)
	}
	if gotRequestID != "" {
		t.Fatalf("x-xai-request-id=%q want empty", gotRequestID)
	}
}
