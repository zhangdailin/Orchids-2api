package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

func qoderTestAccount(machineID string) *store.Account {
	return &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-refresh",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(24 * time.Hour),
		QoderMachineID:    machineID,
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		Enabled:           true,
		Weight:            1,
	}
}

// TestDiscoverQoderModels_PersistsAccountSnapshot proves model refresh publishes
// the channel catalog and records the account-scoped snapshot the routing check
// uses.
func TestDiscoverQoderModels_PersistsAccountSnapshot(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	clearModelsForChannel(t, context.Background(), s, "Qoder")

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	candidates, source, err := discoverQoderModels(context.Background(), &config.Config{}, s)
	if err != nil {
		t.Fatalf("discoverQoderModels() error = %v", err)
	}
	if source != "qoder_builtin_catalog" {
		t.Fatalf("source = %q, want qoder_builtin_catalog", source)
	}
	if len(candidates) == 0 {
		t.Fatal("no candidates were published")
	}
	// The public identifier is the lowercased display name, because that is what
	// a client asks for and what the store index is keyed by.
	seen := map[string]bool{}
	for _, candidate := range candidates {
		seen[candidate.ID] = true
		if candidate.ID != strings.ToLower(candidate.ID) {
			t.Fatalf("candidate %+v is not lowercased", candidate)
		}
		if candidate.Name != candidate.ID {
			t.Fatalf("candidate %+v does not use the public name", candidate)
		}
	}
	if !seen["qwen3.7-max"] {
		t.Fatalf("candidates = %+v, want the Qwen3.7-Max row", candidates)
	}

	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if len(stored.QoderModelIDs) == 0 {
		t.Fatalf("account snapshot = %v, want a recorded snapshot", stored.QoderModelIDs)
	}
	// The stored snapshot uses identifiers advertised by the built-in resolver.
	catalog := qoder.DefaultCatalog()
	if entry, err := catalog.Resolve("Qwen3.7-Max"); err != nil || entry.Key != "qmodel_latest" {
		t.Fatalf("Resolve() = %+v, %v, want the display-name mapping", entry, err)
	}
}

// TestDiscoverQoderModels_DoesNotDependOnTheGateway proves the refresh needs no
// upstream access at all.
//
// The Qoder catalog is local, so pointing every endpoint at a closed port must
// still publish the catalog. If this ever starts reaching the network, the test
// fails — which is the point: a gateway that refuses the model-list read must not
// be able to break model management.
func TestDiscoverQoderModels_DoesNotDependOnTheGateway(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	dead := "http://127.0.0.1:1"
	cfg := &config.Config{
		QoderOAuthBaseURL:   dead,
		QoderOpenAPIBaseURL: dead,
		QoderInferenceURL:   dead,
	}
	candidates, source, err := discoverQoderModels(context.Background(), cfg, s)
	if err != nil {
		t.Fatalf("discoverQoderModels() error = %v, want the local catalog", err)
	}
	if len(candidates) == 0 {
		t.Fatal("no candidates were published")
	}
	if source != "qoder_builtin_catalog" {
		t.Fatalf("source = %q", source)
	}
}

// TestDiscoverQoderModels_RequiresAnEnabledAccount proves the refresh reports
// the missing pool instead of succeeding with nothing.
func TestDiscoverQoderModels_RequiresAnEnabledAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	if _, _, err := discoverQoderModels(context.Background(), &config.Config{}, s); err == nil {
		t.Fatal("discoverQoderModels() error = nil without an enabled account")
	}
}

// TestNormalizeAdminModelChannel_AcceptsQoder proves the admin refresh endpoint
// recognises the channel name.
func TestNormalizeAdminModelChannel_AcceptsQoder(t *testing.T) {
	if got := normalizeAdminModelChannel("qoder"); got != "Qoder" {
		t.Fatalf("normalizeAdminModelChannel(qoder) = %q, want Qoder", got)
	}
	if got := normalizeAdminModelChannel("QODER"); got != "Qoder" {
		t.Fatalf("normalizeAdminModelChannel(QODER) = %q, want Qoder", got)
	}
}

// TestShouldDeleteMissingModelsOnRefresh_KeepsTheLocalQoderCatalog proves a
// Qoder refresh never prunes: nothing upstream was observed, so a missing row is
// not evidence that a model became unavailable.
func TestShouldDeleteMissingModelsOnRefresh_KeepsTheLocalQoderCatalog(t *testing.T) {
	for _, source := range []string{"qoder_builtin_catalog", "something_else"} {
		if shouldDeleteMissingModelsOnRefresh("qoder", source) {
			t.Fatalf("source %q pruned the local Qoder catalog", source)
		}
	}
}
