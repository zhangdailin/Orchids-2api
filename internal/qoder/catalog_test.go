package qoder

import (
	"context"
	"testing"
)

// TestFetchModelsIsLocalAndNeverFailsForAValidClient proves the catalog entry
// point cannot fail because of an upstream read: the OAuth credential is not
// accepted by the gateway's model-list endpoint, so no such read is attempted and
// a device login can never be discarded over it.
func TestFetchModelsIsLocalAndNeverFailsForAValidClient(t *testing.T) {
	t.Parallel()

	// A client pointed at a dead host must still hand back a catalog: if it
	// reached the network at all, this would fail.
	acc := signedTestAccount()
	acc.QoderModelIDs = nil
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")

	catalog, err := client.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels() error = %v, want the built-in catalog", err)
	}
	if catalog == nil || catalog.Len() == 0 {
		t.Fatal("FetchModels() returned no catalog")
	}
	if _, resolveErr := catalog.Resolve("Qwen3.7-Max"); resolveErr != nil {
		t.Fatalf("the built-in catalog cannot resolve its own models: %v", resolveErr)
	}

	// The account snapshot wins when one is stored.
	acc.QoderModelIDs = []string{"kmodel\tKimi-K2.7-Code"}
	snapshotClient := NewFromAccount(acc, nil)
	setTestEndpoints(snapshotClient, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	snapshot, err := snapshotClient.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels() with a snapshot error = %v", err)
	}
	if snapshot.Len() != 1 {
		t.Fatalf("snapshot catalog length = %d, want 1", snapshot.Len())
	}
	if _, resolveErr := snapshot.Resolve("Kimi-K2.7-Code"); resolveErr != nil {
		t.Fatalf("the snapshot catalog cannot resolve its own model: %v", resolveErr)
	}
}
