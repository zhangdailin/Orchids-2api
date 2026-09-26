package qoder

import (
	"orchids-api/internal/store"
	"testing"
)

func TestProfileUpdateInvalidatesRuntimeAndFinalizesLatestTokens(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	if err := client.PrepareRuntimeFields(t.Context()); err != nil {
		t.Fatal(err)
	}
	initial := client.RuntimeFields()
	client.ApplyProfile(Profile{UID: "updated-uid", Name: "updated-name", OrgID: "updated-org"})
	if client.RuntimeFields().Complete() {
		t.Fatal("identity update retained old runtime ciphertext")
	}
	creds := client.currentCredentials()
	if creds.UID != "updated-uid" || creds.OrgID != "updated-org" {
		t.Fatalf("profile snapshot not updated: %+v", creds)
	}
	creds.AccessToken = "rotated-access"
	creds.RefreshToken = "rotated-refresh"
	client.storeCredentials(creds, false, "")
	var account store.Account
	if err := client.FinalizeAccountState(t.Context(), &account); err != nil {
		t.Fatal(err)
	}
	if !client.runtimeTokensMatch(creds) {
		t.Fatal("runtime derived from stale token pair")
	}
	if account.QoderAccessToken != creds.AccessToken || account.QoderRefreshToken != creds.RefreshToken || account.QoderUserID != creds.UID || account.QoderOrganizationID != creds.OrgID {
		t.Fatal("final account state does not match latest snapshot")
	}
	if account.QoderRuntimeInfo == initial.EncryptUserInfo || account.QoderRuntimeInfo == "" {
		t.Fatal("runtime was not regenerated")
	}
	if account.QoderRuntimeInfo != client.RuntimeFields().EncryptUserInfo || account.QoderRuntimeKey != client.RuntimeFields().Key {
		t.Fatal("final account lacks regenerated runtime pair")
	}
}
