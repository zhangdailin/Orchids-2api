package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
)

// qoderCatalogStub serves the signed catalog read for a Qoder account. It is a
// control-plane endpoint, so a successful answer is the credential check.
func qoderCatalogStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/algo/api/v2/model/list") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		for _, header := range []string{"Authorization", "Cosy-Key", "Cosy-MachineId", "Cosy-User", "Cosy-Date"} {
			if r.Header.Get(header) == "" {
				t.Errorf("catalog request is missing %s", header)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"qmodel_latest","display_name":"Qwen3.7-Max","enable":true},{"key":"dfmodel","display_name":"DeepSeek-V4-Flash","enable":true},{"key":"off","display_name":"Off","enable":false}]}`))
	}))
}

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

// TestDiscoverQoderModels_PersistsAccountSnapshot proves model refresh reads the
// account's signed catalog, translates it into channel models and records the
// account-scoped snapshot the routing check uses.
func TestDiscoverQoderModels_PersistsAccountSnapshot(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	clearModelsForChannel(t, context.Background(), s, "Qoder")

	stub := qoderCatalogStub(t)
	defer stub.Close()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cfg := &config.Config{QoderInferenceURL: stub.URL, QoderOpenAPIBaseURL: stub.URL}
	candidates, source, err := discoverQoderModels(context.Background(), cfg, s)
	if err != nil {
		t.Fatalf("discoverQoderModels() error = %v", err)
	}
	if source != "qoder_model_list" {
		t.Fatalf("source = %q, want qoder_model_list", source)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v, want the two enabled rows", candidates)
	}
	// The public identifier is the lowercased display name, because that is what
	// a client asks for and what the store index is keyed by.
	if candidates[0].ID != "qwen3.7-max" || candidates[0].Name != "qwen3.7-max" {
		t.Fatalf("first candidate = %+v, want the lowercased display name", candidates[0])
	}
	for _, candidate := range candidates {
		if candidate.ID == "off" || candidate.ID == "qmodel_latest" {
			t.Fatalf("candidate %+v is not the public form", candidate)
		}
	}

	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if len(stored.QoderModelIDs) != 2 || stored.QoderModelsSyncedAt.IsZero() {
		t.Fatalf("account snapshot = %v (synced %v), want a recorded snapshot", stored.QoderModelIDs, stored.QoderModelsSyncedAt)
	}
	// The snapshot round-trips through the channel's own resolver.
	catalog := qoder.CatalogFromSnapshot(stored.QoderModelIDs)
	if entry, err := catalog.Resolve("Qwen3.7-Max"); err != nil || entry.Key != "qmodel_latest" {
		t.Fatalf("Resolve() = %+v, %v, want the display-name mapping", entry, err)
	}
}

// TestDiscoverQoderModels_ReportsAnAccountFailure proves a broken credential is
// surfaced instead of silently producing an empty catalog.
func TestDiscoverQoderModels_ReportsAnAccountFailure(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer failing.Close()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	cfg := &config.Config{QoderInferenceURL: failing.URL, QoderOpenAPIBaseURL: failing.URL}
	if _, _, err := discoverQoderModels(context.Background(), cfg, s); err == nil {
		t.Fatal("discoverQoderModels() error = nil for a rejected credential")
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

// TestShouldDeleteMissingModelsOnRefresh_CoversQoder proves a model that left the
// account catalog stops being routable.
func TestShouldDeleteMissingModelsOnRefresh_CoversQoder(t *testing.T) {
	if !shouldDeleteMissingModelsOnRefresh("qoder", "qoder_model_list") {
		t.Fatal("qoder catalog refresh must prune models that disappeared")
	}
	if shouldDeleteMissingModelsOnRefresh("qoder", "something_else") {
		t.Fatal("an unknown source must not prune the qoder catalog")
	}
}
