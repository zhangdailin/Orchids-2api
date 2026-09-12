package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

const workBuddyAuthDocument = `{
  "account": {"uid": "07ab88c8-5596-4257-8d21-e9fcbe3a3810"},
  "auth": {
    "accessToken": "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIwN2FiODhjOCJ9.sig",
    "refreshToken": "durable-refresh-token",
    "expiresAt": 1820679206000
  }
}`

func TestNormalizeWorkBuddyCredentials_SplitsDocumentIntoDedicatedFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy", ClientCookie: workBuddyAuthDocument}
	if !NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = false, want true")
	}
	if acc.WorkBuddyRefreshToken != "durable-refresh-token" {
		t.Fatalf("WorkBuddyRefreshToken = %q", acc.WorkBuddyRefreshToken)
	}
	if !strings.HasPrefix(acc.WorkBuddyAccessToken, "eyJhbGciOiJSUzI1NiJ9") {
		t.Fatalf("WorkBuddyAccessToken = %q", acc.WorkBuddyAccessToken)
	}
	if acc.WorkBuddyUID != "07ab88c8-5596-4257-8d21-e9fcbe3a3810" {
		t.Fatalf("WorkBuddyUID = %q", acc.WorkBuddyUID)
	}
	if acc.WorkBuddyExpiresAt.IsZero() {
		t.Fatal("WorkBuddyExpiresAt is zero, want the millisecond expiry converted")
	}
	// The generic credential slots are shared with other channels and must stay
	// empty so the refresh token is never echoed through the account list.
	for name, value := range map[string]string{
		"ClientCookie":  acc.ClientCookie,
		"Token":         acc.Token,
		"RefreshToken":  acc.RefreshToken,
		"SessionCookie": acc.SessionCookie,
	} {
		if value != "" {
			t.Fatalf("%s = %q, want empty", name, value)
		}
	}
}

func TestNormalizeWorkBuddyCredentials_AcceptsBareRefreshToken(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy", ClientCookie: "opaque-refresh-token"}
	if !NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = false, want true")
	}
	if acc.WorkBuddyRefreshToken != "opaque-refresh-token" {
		t.Fatalf("WorkBuddyRefreshToken = %q", acc.WorkBuddyRefreshToken)
	}
}

func TestNormalizeWorkBuddyCredentials_RejectsEmptyInput(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "workbuddy"}
	if NormalizeWorkBuddyCredentials(acc) {
		t.Fatal("NormalizeWorkBuddyCredentials() = true, want false for an empty credential")
	}
}

func TestRedactWorkBuddyOutput_HidesDurableSecrets(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  "visible-access-token",
		WorkBuddyRefreshToken: "durable-refresh-token",
		WorkBuddyUID:          "uid-1",
		Token:                 "legacy-token",
		RefreshToken:          "legacy-refresh",
		SessionCookie:         "legacy-cookie",
		ClientCookie:          "legacy-client-cookie",
	}
	out := RedactWorkBuddyOutput(acc)
	if out == nil {
		t.Fatal("RedactWorkBuddyOutput() = nil")
	}
	if out.WorkBuddyRefreshToken != "" || out.RefreshToken != "" || out.SessionCookie != "" || out.Token != "" {
		t.Fatalf("redacted output still carries a secret: %+v", out)
	}
	if out.WorkBuddyAccessToken != "visible-access-token" || out.WorkBuddyUID != "uid-1" {
		t.Fatalf("redacted output dropped the visible fields: %+v", out)
	}
	if acc.WorkBuddyRefreshToken != "durable-refresh-token" {
		t.Fatal("redaction mutated the source account")
	}
}

func TestWorkBuddyCredentialKey_UsesDurableToken(t *testing.T) {
	t.Parallel()

	key := WorkBuddyCredentialKey(&store.Account{
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  "access",
		WorkBuddyRefreshToken: "refresh",
	})
	if key != "workbuddy:refresh" {
		t.Fatalf("key = %q, want the refresh token to identify the account", key)
	}

	accessOnly := WorkBuddyCredentialKey(&store.Account{WorkBuddyAccessToken: "access-only"})
	if accessOnly != "workbuddy:access-only" {
		t.Fatalf("key = %q, want the access token fallback", accessOnly)
	}

	if empty := WorkBuddyCredentialKey(&store.Account{}); empty != "" {
		t.Fatalf("key = %q, want empty for an account without credentials", empty)
	}
}

func TestVerifyWorkBuddyAccount_RequiresCredential(t *testing.T) {
	t.Parallel()

	_, httpStatus, err := verifyWorkBuddyAccount(context.Background(), &store.Account{AccountType: "workbuddy"}, &config.Config{})
	if err == nil {
		t.Fatal("expected an error for a credential-less account")
	}
	if httpStatus != 400 {
		t.Fatalf("httpStatus = %d, want 400", httpStatus)
	}
}

func TestPreserveWorkBuddyCredentialsOnEdit_KeepsServerSideState(t *testing.T) {
	t.Parallel()

	existing := &store.Account{
		WorkBuddyAccessToken:    "stored-access",
		WorkBuddyRefreshToken:   "stored-refresh",
		WorkBuddyUID:            "stored-uid",
		WorkBuddyModelIDs:       []string{"hy3", "default-model", "gpt-6-astra", "kimi-k3"},
		WorkBuddyModelsSyncedAt: time.Now(),
	}
	edited := &store.Account{AccountType: "workbuddy"}

	PreserveWorkBuddyCredentialsOnEdit(edited, existing)

	if edited.WorkBuddyAccessToken != "stored-access" || edited.WorkBuddyRefreshToken != "stored-refresh" {
		t.Fatalf("credentials were dropped on edit: %+v", edited)
	}
	if len(edited.WorkBuddyModelIDs) != 4 {
		t.Fatalf("WorkBuddyModelIDs = %v, want the stored snapshot", edited.WorkBuddyModelIDs)
	}
	if edited.WorkBuddyModelsSyncedAt.IsZero() {
		t.Fatal("WorkBuddyModelsSyncedAt was reset")
	}
}

func TestWorkBuddyAccessTokenPreview_TruncatesWithoutLeakingRefresh(t *testing.T) {
	t.Parallel()

	preview := WorkBuddyAccessTokenPreview(&store.Account{WorkBuddyAccessToken: "abcdefghijklmnopqrstuvwxyz0123456789"})
	if preview != "abcdefgh...23456789" {
		t.Fatalf("preview = %q", preview)
	}
	if got := WorkBuddyAccessTokenPreview(&store.Account{WorkBuddyRefreshToken: "refresh-only"}); got != "" {
		t.Fatalf("preview = %q, want empty when only a refresh token exists", got)
	}
}

func TestResolveCredentials_MatchesClientResolution(t *testing.T) {
	t.Parallel()

	acc := &store.Account{ClientCookie: workBuddyAuthDocument}
	apiCreds := resolveWorkBuddyCredentials(acc)
	clientCreds := workbuddy.ResolveCredentials(acc)

	if apiCreds.AccessToken != clientCreds.AccessToken || apiCreds.RefreshToken != clientCreds.RefreshToken {
		t.Fatalf("api=%+v client=%+v", apiCreds, clientCreds)
	}
	if apiCreds.UID != clientCreds.UID {
		t.Fatalf("uid api=%q client=%q", apiCreds.UID, clientCreds.UID)
	}
}
