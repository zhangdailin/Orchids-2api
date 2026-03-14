package warp

import (
	"testing"

	"orchids-api/internal/store"
)

func TestResolveRefreshToken_UsesLegacyTokenField(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType: "warp",
		Token:       "legacy-refresh-token",
	}

	if got := ResolveRefreshToken(acc); got != "legacy-refresh-token" {
		t.Fatalf("ResolveRefreshToken()=%q want legacy-refresh-token", got)
	}
}

func TestResolveRefreshToken_PrefersNonJWTOverRuntimeToken(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		Token:        "aaaaaaaaaa.bbbbbbbbbb.cccccccccc",
		ClientCookie: "refresh_token=actual-refresh-token",
	}

	if got := ResolveRefreshToken(acc); got != "actual-refresh-token" {
		t.Fatalf("ResolveRefreshToken()=%q want actual-refresh-token", got)
	}
}
