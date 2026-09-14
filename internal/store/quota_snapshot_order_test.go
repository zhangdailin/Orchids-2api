package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestUpdateAccountStaleRequestCannotOverwriteNewerQuota(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "quota-order:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	acc := &Account{AccountType: "qoder", Enabled: true}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	stale := *acc

	newReset := time.Now().Add(24 * time.Hour).Round(time.Millisecond)
	newSync := time.Now().Round(time.Millisecond)
	fresh := *acc
	fresh.QuotaResetAt = newReset
	fresh.QoderQuota = QoderQuotaSnapshot{Exhausted: true, ResetAt: newReset, SyncedAt: newSync}
	if err := s.UpdateAccount(context.Background(), &fresh); err != nil {
		t.Fatal(err)
	}

	stale.StatusCode = "402"
	stale.LastAttempt = time.Now()
	stale.QuotaResetAt = time.Now().Add(time.Hour)
	stale.QoderQuota = QoderQuotaSnapshot{ResetAt: stale.QuotaResetAt, SyncedAt: newSync.Add(-time.Minute)}
	if err := s.UpdateAccount(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.QuotaResetAt.Equal(newReset) || !got.QoderQuota.ResetAt.Equal(newReset) || !got.QoderQuota.Exhausted {
		t.Fatalf("stale write overwrote quota: reset=%v snapshot=%+v", got.QuotaResetAt, got.QoderQuota)
	}
	if got.StatusCode != "402" {
		t.Fatalf("stale request verdict was lost: status=%q", got.StatusCode)
	}
}
