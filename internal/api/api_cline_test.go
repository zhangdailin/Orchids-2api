package api

import (
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestPreserveClineCredentialsOnEditKeepsCatalogTimestamp(t *testing.T) {
	syncedAt := time.Now().UTC().Truncate(time.Second)
	existing := &store.Account{
		ClineAccessToken:    "access",
		ClineRefreshToken:   "refresh",
		ClineModelIDs:       []string{"model-a"},
		ClineModelsSyncedAt: syncedAt,
	}
	edited := &store.Account{}
	PreserveClineCredentialsOnEdit(edited, existing)
	if len(edited.ClineModelIDs) != 1 || edited.ClineModelIDs[0] != "model-a" {
		t.Fatalf("ClineModelIDs = %v, want stored snapshot", edited.ClineModelIDs)
	}
	if !edited.ClineModelsSyncedAt.Equal(syncedAt) {
		t.Fatalf("ClineModelsSyncedAt = %v, want %v", edited.ClineModelsSyncedAt, syncedAt)
	}
}
