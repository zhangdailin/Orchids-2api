package main

import (
	"context"
	"errors"
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

// TestDiscoverQoderModelsRequiresAnActiveAccount proves the channel reports the
// missing pool instead of publishing anything.
//
// The channel used to answer with a compiled-in catalog here. It no longer has
// one: with no active account nothing is observed, so nothing is published.
func TestDiscoverQoderModelsRequiresAnActiveAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	items, source, err := discoverQoderModels(context.Background(), &config.Config{}, s)
	if err == nil {
		t.Fatalf("discoverQoderModels() items=%+v source=%q want error", items, source)
	}
	if !isNoActiveAccounts(err) {
		t.Fatalf("error=%v want a no-active-account report", err)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
}

// TestDiscoverQoderModelsWithoutAnUpstreamCatalogPublishesNothing proves the
// built-in list is gone: a credential that cannot read the model list leaves the
// channel empty instead of installing a compiled-in catalog.
func TestDiscoverQoderModelsWithoutAnUpstreamCatalogPublishesNothing(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Qoder")

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	// Every endpoint points at a closed port, so the catalog read cannot succeed.
	dead := "http://127.0.0.1:1"
	cfg := &config.Config{
		QoderOAuthBaseURL:   dead,
		QoderOpenAPIBaseURL: dead,
		QoderInferenceURL:   dead,
	}
	items, source, err := discoverQoderModels(ctx, cfg, s)
	if err == nil {
		t.Fatalf("discoverQoderModels() items=%+v source=%q want error", items, source)
	}
	if source != "" {
		t.Fatalf("source=%q want no source for a failed read", source)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
	if strings.Contains(err.Error(), "builtin") {
		t.Fatalf("error=%v must not describe a built-in catalog", err)
	}

	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.Channel), "qoder") {
			t.Fatalf("a compiled-in catalog was published: %+v", model)
		}
	}

	// The credential is still good, so the account is untouched and a later
	// refresh can record the catalog once the read works.
	stored, getErr := s.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatalf("GetAccount() error = %v", getErr)
	}
	if len(stored.QoderModelIDs) != 0 {
		t.Fatalf("account snapshot = %v, want it left empty", stored.QoderModelIDs)
	}
}

// TestDiscoverQoderModelsPublishesTheObservedCatalog proves a successful read is
// what fills model management, and that the account snapshot records the same
// rows so routing resolves against the published catalog.
func TestDiscoverQoderModelsPublishesTheObservedCatalog(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Qoder")

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	acc.QoderModelIDs = []string{
		"qmodel_38max\tQwen3.8-Max",
		"dmodel\tDeepSeek-V4-Pro",
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	prevFetch := fetchQoderUpstreamCatalogForRefresh
	t.Cleanup(func() { fetchQoderUpstreamCatalogForRefresh = prevFetch })
	fetchQoderUpstreamCatalogForRefresh = func(context.Context, *config.Config, *store.Account) (*qoder.Catalog, error) {
		return qoder.CatalogFromSnapshot([]string{"qmodel_38max\tQwen3.8-Max", "dmodel\tDeepSeek-V4-Pro"}), nil
	}

	items, source, err := discoverQoderModels(ctx, &config.Config{}, s)
	if err != nil {
		t.Fatalf("discoverQoderModels() error = %v", err)
	}
	if source != "qoder_upstream_models" {
		t.Fatalf("source=%q want qoder_upstream_models", source)
	}
	if len(items) != 2 {
		t.Fatalf("items=%+v want the two observed rows", items)
	}
	for _, item := range items {
		if !item.Verified {
			t.Fatalf("item %+v is not marked verified", item)
		}
		if item.ID != strings.ToLower(item.ID) {
			t.Fatalf("item %+v is not lowercased", item)
		}
	}

	stored, getErr := s.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatalf("GetAccount() error = %v", getErr)
	}
	if len(stored.QoderModelIDs) == 0 {
		t.Fatal("the observed catalog was not recorded on the account")
	}
}

// TestDiscoverQoderModelsReportsTheReadFailure proves a failed read is reported
// with its cause, so an operator can tell a missing route from a refused
// credential.
func TestDiscoverQoderModelsReportsTheReadFailure(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	prevFetch := fetchQoderUpstreamCatalogForRefresh
	t.Cleanup(func() { fetchQoderUpstreamCatalogForRefresh = prevFetch })
	fetchQoderUpstreamCatalogForRefresh = func(context.Context, *config.Config, *store.Account) (*qoder.Catalog, error) {
		return nil, errors.New("qoder upstream model list unavailable: status=403 code=101")
	}

	_, _, err := discoverQoderModels(ctx, &config.Config{}, s)
	if err == nil {
		t.Fatal("discoverQoderModels() error = nil for a failed read")
	}
	if !strings.Contains(err.Error(), "status=403") {
		t.Fatalf("error=%v does not carry the upstream cause", err)
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

// TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs proves
// pruning follows an observed catalog, and never a locally produced list.
func TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs(t *testing.T) {
	if !shouldDeleteMissingModelsOnRefresh("qoder", "qoder_upstream_models") {
		t.Fatal("an observed Qoder catalog must prune rows it no longer advertises")
	}
	for _, source := range []string{"qoder_builtin_catalog", "warp_cached_models", "test", ""} {
		if shouldDeleteMissingModelsOnRefresh("qoder", source) {
			t.Fatalf("source %q pruned the catalog", source)
		}
	}
}
