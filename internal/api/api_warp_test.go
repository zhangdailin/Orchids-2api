package api

import (
	"testing"

	"orchids-api/internal/store"
)

func TestNormalizeWarpTokenInput_StripsWrappedFormats(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		RefreshToken: "grant_type=refresh_token&refresh_token=token-123",
		ClientCookie: "should-be-cleared",
	}

	normalizeWarpTokenInput(acc)

	if acc.RefreshToken != "token-123" {
		t.Fatalf("RefreshToken=%q want token-123", acc.RefreshToken)
	}
	if acc.ClientCookie != "" {
		t.Fatalf("ClientCookie=%q want empty", acc.ClientCookie)
	}
	if acc.SessionCookie != "" {
		t.Fatalf("SessionCookie=%q want empty", acc.SessionCookie)
	}
}

func TestNormalizeWarpTokenOutput_NormalizesLegacyValue(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		ClientCookie: "foo=1; refresh_token=token-123; Path=/",
	}

	normalized := normalizeWarpTokenOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeWarpTokenOutput returned nil")
	}
	if normalized.RefreshToken != "token-123" {
		t.Fatalf("RefreshToken=%q want token-123", normalized.RefreshToken)
	}
	if normalized.ClientCookie != "" {
		t.Fatalf("ClientCookie=%q want empty", normalized.ClientCookie)
	}
}

func TestNormalizeWarpTokenInput_UsesLegacyTokenField(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType: "warp",
		Token:       "legacy-refresh-token",
	}

	normalizeWarpTokenInput(acc)

	if acc.RefreshToken != "legacy-refresh-token" {
		t.Fatalf("RefreshToken=%q want legacy-refresh-token", acc.RefreshToken)
	}
	if acc.Token != "" {
		t.Fatalf("Token=%q want empty", acc.Token)
	}
}

func TestNormalizeWarpTokenOutput_HidesRuntimeJWT(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:  "warp",
		RefreshToken: "",
		Token:        "aaaaaaaaaa.bbbbbbbbbb.cccccccccc",
	}

	normalized := normalizeWarpTokenOutput(acc)
	if normalized == nil {
		t.Fatal("normalizeWarpTokenOutput returned nil")
	}
	if normalized.RefreshToken != "" {
		t.Fatalf("RefreshToken=%q want empty", normalized.RefreshToken)
	}
	if normalized.Token != "" {
		t.Fatalf("Token=%q want empty", normalized.Token)
	}
}
