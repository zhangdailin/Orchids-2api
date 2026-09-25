package util

import (
	"encoding/base64"
	"testing"
)

func TestJWTEmail(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"workbuddy@example.com"}`))
	if got := JWTEmail(header + "." + payload + ".sig"); got != "workbuddy@example.com" {
		t.Fatalf("JWTEmail() = %q", got)
	}
	if got := JWTEmail("not-a-jwt"); got != "" {
		t.Fatalf("JWTEmail(invalid) = %q", got)
	}
}
