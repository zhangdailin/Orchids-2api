package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestClineModelsSyncedAtJSONAndUpdateMerge(t *testing.T) {
	t.Parallel()

	syncedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	raw, err := json.Marshal(&Account{ClineModelsSyncedAt: syncedAt})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded Account
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !decoded.ClineModelsSyncedAt.Equal(syncedAt) {
		t.Fatalf("JSON round trip ClineModelsSyncedAt = %v, want %v", decoded.ClineModelsSyncedAt, syncedAt)
	}

	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "cline-model-sync:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &Account{
		Name:                "cline",
		AccountType:         "cline",
		Enabled:             true,
		ClineModelIDs:       []string{"old-model"},
		ClineModelsSyncedAt: syncedAt,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	partial := *acc
	partial.ClineModelIDs = nil
	partial.ClineModelsSyncedAt = time.Time{}
	if err := s.UpdateAccount(ctx, &partial); err != nil {
		t.Fatalf("UpdateAccount(partial) error = %v", err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if !got.ClineModelsSyncedAt.Equal(syncedAt) || len(got.ClineModelIDs) != 1 || got.ClineModelIDs[0] != "old-model" {
		t.Fatalf("partial update lost snapshot: ids=%v synced_at=%v", got.ClineModelIDs, got.ClineModelsSyncedAt)
	}

	newer := syncedAt.Add(time.Minute)
	got.ClineModelIDs = []string{"new-model"}
	got.ClineModelsSyncedAt = newer
	if err := s.UpdateAccount(ctx, got); err != nil {
		t.Fatalf("UpdateAccount(newer) error = %v", err)
	}
	stale := *got
	stale.ClineModelIDs = nil
	stale.ClineModelsSyncedAt = syncedAt
	if err := s.UpdateAccount(ctx, &stale); err != nil {
		t.Fatalf("UpdateAccount(stale) error = %v", err)
	}
	got, err = s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount(after stale) error = %v", err)
	}
	if !got.ClineModelsSyncedAt.Equal(newer) {
		t.Fatalf("stale update rewound ClineModelsSyncedAt = %v, want %v", got.ClineModelsSyncedAt, newer)
	}
}
