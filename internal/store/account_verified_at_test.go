package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// TestUpdateAccount_VerifiedAtIsMonotonicPerCredential pins the persistence rule
// the scheduler relies on: an ordinary partial update must not un-verify an
// account, and only an explicit ClearVerifiedAt (credential replacement) drops
// the verdict stamp.
func TestUpdateAccount_VerifiedAtIsMonotonicPerCredential(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "verified-at:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	acc := &Account{
		Name:           "grok-sso",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   "web",
		ClientCookie:   "sso=token",
		Enabled:        true,
		Weight:         1,
		VerifiedAt:     time.Now().Add(-time.Minute),
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	stamp := acc.VerifiedAt

	// A partial update that carries no verdict stamp (request counters, quota
	// rotation) must keep the stored one.
	partial, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	partial.VerifiedAt = time.Time{}
	partial.StatusCode = "401"
	partial.StatusMessage = "upstream rejected the cookie"
	if err := s.UpdateAccount(ctx, partial); err != nil {
		t.Fatalf("UpdateAccount(partial) error = %v", err)
	}
	afterPartial, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !afterPartial.VerifiedAt.Equal(stamp) {
		t.Fatalf("verified_at = %v, want the stored %v", afterPartial.VerifiedAt, stamp)
	}
	if afterPartial.StatusCode != "401" || afterPartial.StatusMessage == "" {
		t.Fatalf("status write lost: %q / %q", afterPartial.StatusCode, afterPartial.StatusMessage)
	}

	// Replacing the credential drops it so the new credential is verified.
	afterPartial.ClientCookie = "sso=replacement"
	afterPartial.ClearVerifiedAt = true
	if err := s.UpdateAccount(ctx, afterPartial); err != nil {
		t.Fatalf("UpdateAccount(replacement) error = %v", err)
	}
	afterEdit, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !afterEdit.VerifiedAt.IsZero() {
		t.Fatalf("verified_at = %v, want cleared for a replaced credential", afterEdit.VerifiedAt)
	}
	if afterEdit.ClearVerifiedAt {
		t.Fatal("ClearVerifiedAt leaked into the stored record")
	}

	// A fresh verdict is stored normally.
	afterEdit.VerifiedAt = time.Now()
	if err := s.UpdateAccount(ctx, afterEdit); err != nil {
		t.Fatalf("UpdateAccount(verdict) error = %v", err)
	}
	final, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if final.VerifiedAt.IsZero() {
		t.Fatal("a new verdict timestamp was not persisted")
	}
}
