package grok

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
)

// signerFixture is a valid grok2api statsig id: base64 that decodes to 70 bytes.
func signerFixture(seed byte) string {
	raw := make([]byte, 70)
	for i := range raw {
		raw[i] = seed
	}
	return base64.RawStdEncoding.EncodeToString(raw)
}

func statsigTestPage(meta string) string {
	return `<!doctype html><html><head><meta name="grok-site-verification" content="` + meta + `">` +
		`<meta name="other" content="ignored"></head><body>grok</body></html>`
}

func TestExtractStatsigMetaContent(t *testing.T) {
	if got, err := extractStatsigMetaContent([]byte(statsigTestPage("verify-me"))); err != nil || got != "verify-me" {
		t.Fatalf("meta=%q err=%v", got, err)
	}
	// A page without the tag is a protocol failure, not an empty value.
	if _, err := extractStatsigMetaContent([]byte(`<html><head></head></html>`)); err == nil {
		t.Fatal("a page without the verification meta was accepted")
	}
	// The tag is found regardless of case and attribute order.
	page := `<HTML><HEAD><META CONTENT="order-independent" NAME="Grok-Site-Verification"></HEAD></HTML>`
	if got, err := extractStatsigMetaContent([]byte(page)); err != nil || got != "order-independent" {
		t.Fatalf("meta=%q err=%v", got, err)
	}
}

func TestValidStatsigID(t *testing.T) {
	if !validStatsigID(signerFixture(7)) {
		t.Fatal("a 70-byte id was rejected")
	}
	if validStatsigID(base64.RawStdEncoding.EncodeToString(make([]byte, 69))) {
		t.Fatal("a 69-byte id was accepted")
	}
	if validStatsigID("not-base64!!") || validStatsigID("") {
		t.Fatal("an invalid id was accepted")
	}
}

func TestValidateStatsigSignerURL(t *testing.T) {
	for _, allowed := range []string{
		"https://grok.wodf.de/sign",
		"https://signer.example.com:443/sign",
		"http://127.0.0.1:8080/sign",
		"http://statsig-signer/sign",
	} {
		if err := validateStatsigSignerURL(allowed); err != nil {
			t.Fatalf("validateStatsigSignerURL(%q) = %v, want nil", allowed, err)
		}
	}
	for _, refused := range []string{
		"", "not a url", "ftp://example.com/sign", "https://example.com:8443/sign",
		"http://example.com/sign", "https://user:pass@example.com/sign",
		"https://example.com/sign?token=1", "https://example.com/sign#frag",
	} {
		if err := validateStatsigSignerURL(refused); err == nil {
			t.Fatalf("validateStatsigSignerURL(%q) was accepted", refused)
		}
	}
}

// The signer reads the account's page, posts the meta content to the signing
// endpoint, and caches the result per method+path for an hour.
func TestStatsigSignerSignsAndCaches(t *testing.T) {
	pageCalls := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageCalls++
		if r.URL.Path != "/index" {
			t.Errorf("page path=%s want /index", r.URL.Path)
		}
		if cookie := r.Header.Get("Cookie"); !strings.Contains(cookie, "sso=token") {
			t.Errorf("page read did not carry the account cookie: %q", cookie)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, statsigTestPage("meta-content"))
	}))
	defer page.Close()

	signCalls := 0
	var payload map[string]interface{}
	signer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signCalls++
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": signerFixture(9)})
	}))
	defer signer.Close()

	s := newStatsigSigner()
	do := func(request *http.Request) (*http.Response, error) { return page.Client().Do(request) }
	ctx := context.Background()

	value, source, err := s.signature(ctx, do, page.URL, signer.URL+"/sign", "sso=token", http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if err != nil || value != signerFixture(9) || source != "refresh" {
		t.Fatalf("value=%q source=%q err=%v", value, source, err)
	}
	environment, _ := payload["environment"].(map[string]interface{})
	if environment["metaContent"] != "meta-content" || payload["path"] != "/rest/app-chat/conversations/new" || payload["method"] != "POST" {
		t.Fatalf("signer payload=%#v", payload)
	}

	// A second request for the same endpoint is served from cache.
	value, source, err = s.signature(ctx, do, page.URL, signer.URL+"/sign", "sso=token", http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if err != nil || value != signerFixture(9) || source != "cache" {
		t.Fatalf("second value=%q source=%q err=%v", value, source, err)
	}
	if signCalls != 1 || pageCalls != 1 {
		t.Fatalf("signCalls=%d pageCalls=%d want one of each", signCalls, pageCalls)
	}

	// A different path is a different signature.
	if _, source, err = s.signature(ctx, do, page.URL, signer.URL+"/sign", "sso=token", http.MethodPost, page.URL+"/rest/other"); err != nil || source != "refresh" {
		t.Fatalf("distinct path source=%q err=%v", source, err)
	}
	if signCalls != 2 {
		t.Fatalf("signCalls=%d want 2", signCalls)
	}

	// Invalidation (the anti-bot path) forces a fresh signature.
	s.invalidate(page.URL, signer.URL+"/sign", http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if _, source, err = s.signature(ctx, do, page.URL, signer.URL+"/sign", "sso=token", http.MethodPost, page.URL+"/rest/app-chat/conversations/new"); err != nil || source != "refresh" {
		t.Fatalf("after invalidate source=%q err=%v", source, err)
	}
}

// An expired entry still answers with the stale value when re-signing fails: a
// rejected id is more useful to the caller than none.
func TestStatsigSignerFallsBackToStaleValue(t *testing.T) {
	value := signerFixture(3)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, statsigTestPage("meta"))
	}))
	defer page.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	s := newStatsigSigner()
	now := time.Now()
	s.now = func() time.Time { return now }
	key, _, err := statsigSignatureKey(page.URL, failing.URL, http.MethodPost, page.URL+"/rest/x")
	if err != nil {
		t.Fatal(err)
	}
	s.store(key, value, now.Add(time.Minute), now)

	// Past the TTL, with a signer that cannot answer.
	now = now.Add(2 * time.Hour)
	do := func(request *http.Request) (*http.Response, error) {
		return page.Client().Do(request)
	}
	got, source, err := s.signature(context.Background(), do, page.URL, failing.URL, "", http.MethodPost, page.URL+"/rest/x")
	if err != nil || got != value || source != "stale" {
		t.Fatalf("value=%q source=%q err=%v", got, source, err)
	}
}

// A signer that answers with something that is not a statsig id must not be
// trusted.
func TestStatsigSignerRejectsInvalidResponse(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, statsigTestPage("meta"))
	}))
	defer page.Close()
	signer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": "tooshort"})
	}))
	defer signer.Close()

	s := newStatsigSigner()
	do := func(request *http.Request) (*http.Response, error) { return page.Client().Do(request) }
	if _, _, err := s.signature(context.Background(), do, page.URL, signer.URL, "", http.MethodPost, page.URL+"/rest/x"); err == nil {
		t.Fatal("an invalid signer response was accepted")
	}
}

// The client sends the signed header when a signer is configured, and omits it
// when signing is off or fails.
func TestClientResolvesStatsigHeaderForRequest(t *testing.T) {
	value := signerFixture(11)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, statsigTestPage("meta"))
	}))
	defer page.Close()
	signer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": value})
	}))
	defer signer.Close()

	signerURL := signer.URL
	client := &Client{cfg: &config.Config{GrokStatsigSignerURL: &signerURL, GrokAPIBaseURL: page.URL}}
	client.httpClient = page.Client()
	got := client.statsigIDForRequest(context.Background(), "sso=token", http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if got != value {
		t.Fatalf("signed id=%q want %q", got, value)
	}
	// A manual value is still used when signing cannot produce one.
	manual := signerFixture(5)
	// Signing turned off explicitly: no signer URL and no manual value means the
	// header is omitted rather than fabricated.
	disabled := ""
	empty := &Client{cfg: &config.Config{GrokStatsigSignerURL: &disabled}}
	if got := empty.statsigIDForRequest(context.Background(), "sso=token", http.MethodPost, "https://grok.com/rest"); got != "" {
		t.Fatalf("id=%q want empty", got)
	}
	// Unset means grok2api's own default endpoint, so an upgraded deployment signs
	// without being configured: pointed at the test page it must sign there.
	unsetClient := &Client{cfg: &config.Config{GrokAPIBaseURL: page.URL}}
	unsetClient.httpClient = page.Client()
	if got := unsetClient.statsigSignerURL(); got != DefaultStatsigSignerURL {
		t.Fatalf("unset signer URL=%q want the grok2api default", got)
	}
	// A failing signer falls back to the manual value, not to an error.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	failingURL := failing.URL
	fallback := &Client{cfg: &config.Config{GrokStatsigSignerURL: &failingURL, GrokStatsigID: manual, GrokAPIBaseURL: page.URL}}
	fallback.httpClient = page.Client()
	if got := fallback.statsigIDForRequest(context.Background(), "sso=token", http.MethodPost, page.URL+"/rest"); got != manual {
		t.Fatalf("fallback id=%q want the manual value", got)
	}
}

// An unset endpoint means grok2api's own signer, and the client reports exactly
// that, so an upgraded deployment signs without being configured.
func TestStatsigSigningDefaultsToGrok2APIEndpoint(t *testing.T) {
	client := &Client{cfg: &config.Config{}}
	if got := client.statsigSignerURL(); got != DefaultStatsigSignerURL {
		t.Fatalf("default signer=%q want %q", got, DefaultStatsigSignerURL)
	}
	// The default endpoint passes this gateway's own URL validation, so it is not
	// refused where it is used.
	if err := ValidateStatsigSignerURL(DefaultStatsigSignerURL); err != nil {
		t.Fatalf("the default signer URL was refused: %v", err)
	}
	// An explicit empty string is the documented opt-out.
	disabled := ""
	if got := (&Client{cfg: &config.Config{GrokStatsigSignerURL: &disabled}}).statsigSignerURL(); got != "" {
		t.Fatalf("opt-out signer=%q want empty", got)
	}
	// A configured endpoint wins.
	custom := "https://signer.internal.example/sign"
	if got := (&Client{cfg: &config.Config{GrokStatsigSignerURL: &custom}}).statsigSignerURL(); got != custom {
		t.Fatalf("configured signer=%q want %q", got, custom)
	}
}

// The signer reads the account's page through the client's own transport, and a
// page without the verification meta is a signing failure rather than a silent
// empty signature.
func TestStatsigSigningUsesTheDefaultEndpointWhenUnset(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, statsigTestPage("meta"))
	}))
	defer page.Close()
	value := signerFixture(21)
	var signedPath string
	signer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		signedPath, _ = payload["path"].(string)
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": value})
	}))
	defer signer.Close()

	// The client points at the mock page, but leaves the signer endpoint unset, so
	// the default is used: point the default at the mock through the package
	// variable the tests own.
	previous := statsigDefaultEndpointOverride
	statsigDefaultEndpointOverride = signer.URL
	defer func() { statsigDefaultEndpointOverride = previous }()

	client := &Client{cfg: &config.Config{GrokAPIBaseURL: page.URL}}
	client.httpClient = page.Client()
	got := client.statsigIDForRequest(context.Background(), "sso=token", http.MethodPost, page.URL+"/rest/app-chat/conversations/new")
	if got != value {
		t.Fatalf("signed id=%q want %q", got, value)
	}
	if signedPath != "/rest/app-chat/conversations/new" {
		t.Fatalf("signer path=%q", signedPath)
	}
}
