package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetVersionIsContentDerived pins the property that replaced the
// hand-maintained ?v= strings: the version must come from the embedded bytes,
// so a deployment that changes a script also changes the URL it is fetched
// under. When the version was a literal, removing a provider from the UI left
// every browser on the cached copy of the previous one.
func TestAssetVersionIsContentDerived(t *testing.T) {
	version := AssetVersion()
	if len(version) != 12 {
		t.Fatalf("AssetVersion() = %q, want a 12-character hash", version)
	}
	if again := AssetVersion(); again != version {
		t.Fatalf("AssetVersion() is not stable: %q then %q", version, again)
	}
	if version == assetVersionPlaceholder {
		t.Fatalf("AssetVersion() returned the placeholder %q", version)
	}
}

// TestLoginPageResolvesAssetVersion covers the static login page, which is the
// one asset URL that cannot read PageData.
func TestLoginPageResolvesAssetVersion(t *testing.T) {
	page, err := LoginPage()
	if err != nil {
		t.Fatalf("LoginPage() error = %v", err)
	}
	if bytes.Contains(page, []byte(assetVersionPlaceholder)) {
		t.Fatalf("LoginPage() still carries %q", assetVersionPlaceholder)
	}
	want := []byte("main.css?v=" + AssetVersion())
	if !bytes.Contains(page, want) {
		t.Fatalf("LoginPage() does not link %s", want)
	}
	if !strings.Contains(string(page), "<title>") {
		t.Fatal("LoginPage() does not look like the login page")
	}
}

func TestStaticHandlerCachePolicy(t *testing.T) {
	handler := StaticHandler()
	tests := []struct {
		path string
		want string
	}{
		{"/css/main.css?v=release", "public, max-age=31536000, immutable"},
		{"/js/common.js?v=release", "public, max-age=31536000, immutable"},
		{"/css/main.css", "no-cache"},
		{"/login.html", "no-cache"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)
			handler.ServeHTTP(recorder, request)
			if got := recorder.Header().Get("Cache-Control"); got != tt.want {
				t.Fatalf("Cache-Control = %q, want %q", got, tt.want)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
		})
	}
}
