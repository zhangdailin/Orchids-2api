package grok

import (
	"encoding/base64"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func jwtWithClaims(t *testing.T, claims string) string {
	t.Helper()
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

func TestCLIHeadersUseOfficialBuildIdentity(t *testing.T) {
	client := NewCLIClient(&config.Config{})
	headers := client.cliHeaders(nil, "test-access-token")
	if got := headers.Get("Authorization"); got != "Bearer test-access-token" {
		t.Fatalf("Authorization=%q", got)
	}
	if got := headers.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
		t.Fatalf("X-XAI-Token-Auth=%q", got)
	}
	if got := headers.Get("x-grok-client-identifier"); got != "grok-shell" {
		t.Fatalf("x-grok-client-identifier=%q", got)
	}
	if got := headers.Get("x-grok-client-version"); got != "1.0.4" {
		t.Fatalf("x-grok-client-version=%q", got)
	}
	if got := headers.Get("User-Agent"); got != "grok-shell/1.0.4 (linux; x86_64)" {
		t.Fatalf("User-Agent=%q", got)
	}
}

func TestApplyCLIOAuthIdentity(t *testing.T) {
	acc := &store.Account{OAuthAccessToken: jwtWithClaims(t, `{"sub":"user-1","email":"user@example.com","team_id":"team-1"}`)}
	if !ApplyCLIOAuthIdentity(acc) {
		t.Fatal("expected identity fields to be applied")
	}
	if acc.UserID != "user-1" || acc.Email != "user@example.com" || acc.TeamID != "team-1" {
		t.Fatalf("account=%+v", acc)
	}
}

func TestApplyCLIOAuthIdentityTokenUsesIDTokenEmailForGenericLogin(t *testing.T) {
	acc := &store.Account{Name: "grok-device-login", OAuthAccessToken: jwtWithClaims(t, `{"sub":"user-1","team_id":"team-1"}`)}
	ApplyCLIOAuthIdentity(acc)
	if !ApplyCLIOAuthIdentityToken(acc, jwtWithClaims(t, `{"email":"oauth@example.com","preferred_username":"ignored@example.com"}`)) {
		t.Fatal("expected id_token identity fields to be applied")
	}
	if acc.Email != "oauth@example.com" || acc.Name != "oauth@example.com" || acc.UserID != "user-1" {
		t.Fatalf("account=%+v", acc)
	}
}
