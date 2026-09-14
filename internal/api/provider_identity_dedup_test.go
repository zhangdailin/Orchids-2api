package api

import (
	"context"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestFindDuplicateAccountUsesStableProviderIdentityAfterTokenRotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *store.Account
		login    *store.Account
	}{
		{
			name:     "workbuddy uid",
			existing: &store.Account{AccountType: "workbuddy", WorkBuddyUID: "wb-user", WorkBuddyRefreshToken: "old-refresh", Enabled: true},
			login:    &store.Account{AccountType: "workbuddy", WorkBuddyUID: "wb-user", WorkBuddyRefreshToken: "new-refresh", Enabled: true},
		},
		{
			name:     "qoder user id",
			existing: &store.Account{AccountType: "qoder", QoderUserID: "q-user", QoderRefreshToken: "old-refresh", Enabled: true},
			login:    &store.Account{AccountType: "qoder", QoderUserID: "q-user", QoderRefreshToken: "new-refresh", Enabled: true},
		},
		{
			name:     "grok oauth user id",
			existing: &store.Account{AccountType: "grok", CredentialType: "oauth", UserID: "grok-user", OAuthRefreshToken: "old-refresh", Enabled: true},
			login:    &store.Account{AccountType: "grok", CredentialType: "oauth", UserID: "grok-user", OAuthRefreshToken: "new-refresh", Enabled: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t, "identity-dedup:")
			if err := s.CreateAccount(context.Background(), tc.existing); err != nil {
				t.Fatal(err)
			}
			a := New(s, "", "", &config.Config{})
			got, err := a.findDuplicateAccountByCredential(context.Background(), tc.login, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.ID != tc.existing.ID {
				t.Fatalf("duplicate = %+v, want account %d", got, tc.existing.ID)
			}
		})
	}
}

func TestStableProviderIdentityNeverCrossesProviders(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t, "identity-isolation:")
	existing := &store.Account{AccountType: "workbuddy", WorkBuddyUID: "same-user", WorkBuddyRefreshToken: "wb-token", Enabled: true}
	if err := s.CreateAccount(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	a := New(s, "", "", &config.Config{})
	got, err := a.findDuplicateAccountByCredential(context.Background(), &store.Account{
		AccountType:       "qoder",
		QoderUserID:       "same-user",
		QoderRefreshToken: "q-token",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("cross-provider account was treated as duplicate: %+v", got)
	}
}
