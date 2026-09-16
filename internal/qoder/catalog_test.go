package qoder

import (
	"context"
	"errors"
	"testing"
)

// TestFetchModelsReadsOnlyTheObservedSnapshot pins the catalog contract: the
// read is a pure snapshot lookup.
//
// No network is attempted and there is no compiled-in fallback, so a client
// pointed at a dead host with no snapshot reports that state instead of
// inventing a catalog.
func TestFetchModelsReadsOnlyTheObservedSnapshot(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderModelIDs = nil
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")

	if _, err := client.FetchModels(context.Background()); !errors.Is(err, ErrNoUpstreamCatalog) {
		t.Fatalf("FetchModels() with no snapshot error = %v, want ErrNoUpstreamCatalog", err)
	}

	// A recorded snapshot is served verbatim.
	acc.QoderModelIDs = []string{
		"kmodel\tKimi-K2.7-Code",
		"qmodel_38max\tQwen3.8-Max",
	}
	snapshotClient := NewFromAccount(acc, nil)
	setTestEndpoints(snapshotClient, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	snapshot, err := snapshotClient.FetchModels(context.Background())
	if err != nil {
		t.Fatalf("FetchModels() with a snapshot error = %v", err)
	}
	if snapshot.Len() != 2 {
		t.Fatalf("snapshot catalog length = %d, want exactly the 2 recorded rows", snapshot.Len())
	}
	if _, resolveErr := snapshot.Resolve("Kimi-K2.7-Code"); resolveErr != nil {
		t.Fatalf("the snapshot catalog cannot resolve its own model: %v", resolveErr)
	}
	// A model the snapshot does not carry must not resolve, even though a
	// compiled-in list used to contain it.
	if _, resolveErr := snapshot.Resolve("GLM-5.3-Flash"); resolveErr == nil {
		t.Fatal("a model absent from the snapshot resolved anyway")
	}
}

// TestLoadCatalogWithoutSnapshotYieldsErrNoUpstreamCatalog proves the chat path
// has no compiled-in catalog either: routing against an empty catalog reports
// that no catalog was observed.
func TestLoadCatalogWithoutSnapshotYieldsErrNoUpstreamCatalog(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	acc.QoderModelIDs = nil
	client := NewFromAccount(acc, nil)
	if _, err := client.loadCatalog().Resolve("Qwen3.7-Max"); !errors.Is(err, ErrNoUpstreamCatalog) {
		t.Fatalf("loadCatalog().Resolve() error = %v, want ErrNoUpstreamCatalog", err)
	}
}
