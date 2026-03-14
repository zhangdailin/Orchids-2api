package warp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

func TestNormalizeRefreshToken_ExtractsCommonFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "plain", raw: " token-123 ", want: "token-123"},
		{name: "legacy composite", raw: "mail@example.com----device-id----token-123", want: "token-123"},
		{name: "form encoded", raw: "grant_type=refresh_token&refresh_token=token-123", want: "token-123"},
		{name: "cookie fragment", raw: "foo=1; refresh_token=token-123; Path=/", want: "token-123"},
		{name: "json snake", raw: `{"refresh_token":"token-123"}`, want: "token-123"},
		{name: "json camel", raw: `{"auth":{"refreshToken":"token-123"}}`, want: "token-123"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeRefreshToken(tt.raw); got != tt.want {
				t.Fatalf("normalizeRefreshToken(%q)=%q want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSessionRefresh_UsesFirebaseWhenSuccessful(t *testing.T) {
	t.Parallel()

	sess := &session{refreshToken: "token-123"}
	var seen string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen = req.URL.String()
			if req.URL.String() != warpFirebaseURL {
				t.Fatalf("unexpected refresh URL: %s", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"id_token":"firebase-jwt","refresh_token":"rotated-token","expires_in":"3600"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	if err := sess.refresh(context.Background(), client); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if seen != warpFirebaseURL {
		t.Fatalf("endpoint=%q want %q", seen, warpFirebaseURL)
	}
	if sess.currentJWT() != "firebase-jwt" {
		t.Fatalf("currentJWT=%q want firebase-jwt", sess.currentJWT())
	}
	if sess.currentRefreshToken() != "rotated-token" {
		t.Fatalf("currentRefreshToken=%q want rotated-token", sess.currentRefreshToken())
	}
}
