package warp

import (
	"strings"
	"testing"
)

func TestWarpFirebaseURLsUseEnvironmentKey(t *testing.T) {
	t.Setenv(warpFirebaseAPIKeyEnv, " replacement-key ")

	tokenURL, err := warpFirebaseTokenURL()
	if err != nil {
		t.Fatalf("warpFirebaseTokenURL() error = %v", err)
	}
	if tokenURL != warpFirebaseTokenEndpoint+"?key=replacement-key" {
		t.Fatalf("token URL = %q", tokenURL)
	}

	customTokenURL, err := warpFirebaseCustomTokenURL()
	if err != nil {
		t.Fatalf("warpFirebaseCustomTokenURL() error = %v", err)
	}
	if customTokenURL != warpFirebaseCustomTokenEndpoint+"?key=replacement-key" {
		t.Fatalf("custom token URL = %q", customTokenURL)
	}
}

func TestWarpFirebaseURLRequiresEnvironmentKey(t *testing.T) {
	t.Setenv(warpFirebaseAPIKeyEnv, "")

	_, err := warpFirebaseTokenURL()
	if err == nil || !strings.Contains(err.Error(), warpFirebaseAPIKeyEnv) {
		t.Fatalf("warpFirebaseTokenURL() error = %v, want missing environment variable", err)
	}
}
