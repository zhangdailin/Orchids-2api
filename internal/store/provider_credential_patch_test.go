package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newProviderPatchStore(t *testing.T) *Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "provider-patch:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	return s
}

func TestProviderCredentialPatchesPreserveConcurrentAccountFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newProviderPatchStore(t)

	wb := &Account{
		AccountType: "workbuddy", Enabled: true, Name: "before", StatusCode: "429",
		UsageCurrent: 17, WorkBuddyAccessToken: "access-old", WorkBuddyRefreshToken: "refresh-old",
	}
	if err := s.CreateAccount(ctx, wb); err != nil {
		t.Fatal(err)
	}
	admin := *wb
	admin.Name = "after"
	admin.StatusCode = ""
	admin.WorkBuddyRefreshToken = "stale-refresh"
	if err := s.UpdateAccount(ctx, &admin); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkBuddyCredentials(ctx, wb.ID, WorkBuddyCredentialPatch{
		ExpectedRefreshToken: "refresh-old",
		AccessToken:          "access-new",
		RefreshToken:         "refresh-new",
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	storedWB, err := s.GetAccount(ctx, wb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedWB.Name != "after" || storedWB.UsageCurrent != 17 {
		t.Fatalf("credential patch lost unrelated fields: %+v", storedWB)
	}
	if storedWB.WorkBuddyRefreshToken != "refresh-new" {
		t.Fatalf("refresh token = %q", storedWB.WorkBuddyRefreshToken)
	}

	qoder := &Account{
		AccountType: "qoder", Enabled: true, Name: "qoder", UsageLimit: 100,
		QoderAccessToken: "q-access-old", QoderRefreshToken: "q-refresh-old", QoderMachineID: "machine",
	}
	if err := s.CreateAccount(ctx, qoder); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateQoderAccount(ctx, qoder.ID, QoderAccountPatch{
		ExpectedRefreshToken: "q-refresh-old",
		AccessToken:          "q-access-new",
		RefreshToken:         "q-refresh-new",
		RuntimeInfo:          "runtime",
		RuntimeKey:           "key",
	}); err != nil {
		t.Fatal(err)
	}
	storedQoder, err := s.GetAccount(ctx, qoder.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedQoder.UsageLimit != 100 || storedQoder.QoderRefreshToken != "q-refresh-new" || storedQoder.QoderRuntimeKey != "key" {
		t.Fatalf("qoder patch produced the wrong state: %+v", storedQoder)
	}
}

func TestProviderCredentialPatchRejectsStaleRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newProviderPatchStore(t)
	acc := &Account{AccountType: "qoder", Enabled: true, QoderRefreshToken: "refresh-current"}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	err := s.UpdateQoderAccount(ctx, acc.ID, QoderAccountPatch{
		ExpectedRefreshToken: "refresh-stale",
		RefreshToken:         "refresh-would-overwrite",
	})
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("error = %v, want a concurrent credential rejection", err)
	}
	stored, getErr := s.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.QoderRefreshToken != "refresh-current" {
		t.Fatalf("stale patch overwrote token: %q", stored.QoderRefreshToken)
	}
}
