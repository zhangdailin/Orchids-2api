package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

// wantedProviderRoutes lists the conversation routes the compatibility table
// declares for one plane, which is what the seeding pass must publish.
func wantedProviderRoutes(upstream grok.UpstreamKind) []string {
	out := make([]string, 0, 8)
	for _, spec := range grok.SupportedModels {
		if spec.Upstream != upstream || !spec.SupportsConversation() || grok.IsDeprecatedModelID(spec.ID) {
			continue
		}
		out = append(out, spec.ID)
	}
	return out
}

func createGrokAccount(t *testing.T, s *store.Store, acc *store.Account) *store.Account {
	t.Helper()
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
	}
	return acc
}

// TestEnsureGrokProviderRouteModelsPublishesOnlyConfiguredPlanes pins the two
// halves of the decision: a plane with accounts gets its whole route table, and a
// plane without one gets nothing. Build is never seeded from the table — its rows
// come from the account's own catalog read.
func TestEnsureGrokProviderRouteModelsPublishesOnlyConfiguredPlanes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")

	added, err := ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("ensureGrokProviderRouteModels() error = %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added=%v want none without any account", added)
	}

	createGrokAccount(t, s, &store.Account{
		Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: grok.ProviderBuild,
		OAuthAccessToken: "access", OAuthRefreshToken: "refresh", Enabled: true,
	})
	added, err = ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("ensureGrokProviderRouteModels() error = %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added=%v: a Build account must not seed another plane", added)
	}

	createGrokAccount(t, s, &store.Account{
		Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
		ClientCookie: "sso=web-token", Subscription: "super", Enabled: true,
	})
	added, err = ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("ensureGrokProviderRouteModels() error = %v", err)
	}
	wantWeb := wantedProviderRoutes(grok.UpstreamAppChat)
	if len(wantWeb) == 0 {
		t.Fatal("the compatibility table declares no AppChat conversation route to publish")
	}
	if strings.Join(added, ",") != strings.Join(wantWeb, ",") {
		t.Fatalf("added=%v want %v", added, wantWeb)
	}
	for _, id := range wantWeb {
		row, err := s.GetModelByChannelAndModelID(ctx, "grok", id)
		if err != nil || row == nil {
			t.Fatalf("row %q missing: %v", id, err)
		}
		if !strings.EqualFold(row.Provider, grok.ProviderWeb) || !strings.EqualFold(row.UpstreamModel, id) {
			t.Fatalf("row %q route = %s/%s", id, row.Provider, row.UpstreamModel)
		}
		if !row.Verified || !row.Status.Enabled() || row.Origin != "catalog" {
			t.Fatalf("row %q flags = verified:%v status:%v origin:%q", id, row.Verified, row.Status, row.Origin)
		}
		if !row.SupportsCapability(store.CapabilityChat) {
			t.Fatalf("row %q capabilities = %v want chat", id, row.Capabilities)
		}
	}
	// Re-running the pass is a no-op: the routes already exist.
	again, err := ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("second pass error = %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second pass added=%v want none", again)
	}

	companion := createGrokAccount(t, s, &store.Account{
		Name: "web · Console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole,
		GrokSSOParentID: 1, ClientCookie: "sso=web-token", Enabled: true,
	})
	added, err = ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("ensureGrokProviderRouteModels() error = %v", err)
	}
	wantConsole := wantedProviderRoutes(grok.UpstreamConsole)
	if strings.Join(added, ",") != strings.Join(wantConsole, ",") {
		t.Fatalf("added=%v want %v", added, wantConsole)
	}
	row, err := s.GetModelByChannelAndModelID(ctx, "grok", wantConsole[0])
	if err != nil || row == nil {
		t.Fatalf("console row missing: %v", err)
	}
	if !strings.EqualFold(row.Provider, grok.ProviderConsole) || !row.Verified {
		t.Fatalf("console row = %+v", row)
	}
	if companion.GrokSSOParentID != 1 {
		t.Fatalf("companion setup changed: %+v", companion)
	}
	// Nothing may be published under a bare deprecated name.
	for _, id := range append(append([]string{}, wantWeb...), wantConsole...) {
		if grok.IsDeprecatedModelID(id) {
			t.Fatalf("published deprecated id %q", id)
		}
		if _, err := s.GetModelByChannelAndModelID(ctx, "grok", id); err != nil {
			t.Fatalf("row %q missing: %v", id, err)
		}
	}
}

// TestEnsureGrokProviderRouteModelsNeverRewritesAnExistingRow keeps the operator
// in charge: a row that exists — disabled, renamed or routed by hand — is left
// exactly as it is, and the pass reports no creation for it.
func TestEnsureGrokProviderRouteModelsNeverRewritesAnExistingRow(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	createGrokAccount(t, s, &store.Account{
		Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
		ClientCookie: "sso=web-token", Enabled: true,
	})
	operatorRow := &store.Model{
		Channel: "grok", ModelID: "grok-chat-auto", Name: "内部 Auto", Provider: "web",
		UpstreamModel: "grok-chat-auto", Capabilities: []string{"chat"}, Origin: "manual",
		Status: store.ModelStatusOffline, Verified: true,
	}
	if err := s.CreateModel(ctx, operatorRow); err != nil {
		t.Fatal(err)
	}

	added, err := ensureGrokProviderRouteModels(ctx, s)
	if err != nil {
		t.Fatalf("ensureGrokProviderRouteModels() error = %v", err)
	}
	for _, id := range added {
		if id == "grok-chat-auto" {
			t.Fatal("an existing row must not be reported as created")
		}
	}
	persisted, err := s.GetModelByChannelAndModelID(ctx, "grok", "grok-chat-auto")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ID != operatorRow.ID || persisted.Name != "内部 Auto" || persisted.Status != store.ModelStatusOffline || persisted.Origin != "manual" {
		t.Fatalf("operator row was rewritten: %+v", persisted)
	}
}

// TestGrokRefreshWithoutBuildAccountsStillPublishesProviderRoutes covers the
// deployment whose Grok accounts all sit on the Console plane: the Build read has
// no account to make, and the refresh must still publish the routes that make
// those accounts usable instead of reporting nothing to do.
func TestGrokRefreshWithoutBuildAccountsStillPublishesProviderRoutes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	createGrokAccount(t, s, &store.Account{
		Name: "web · Console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole,
		GrokSSOParentID: 7, ClientCookie: "sso=web-token", Enabled: true,
	})

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 2)
	if err != nil {
		t.Fatalf("syncModelsForChannelConcurrent() error = %v", err)
	}
	want := wantedProviderRoutes(grok.UpstreamConsole)
	if result == nil || result.Outcome != "routes_only" || result.Skipped {
		t.Fatalf("result=%+v want a routes-only success", result)
	}
	if strings.Join(result.ProviderRoutes, ",") != strings.Join(want, ",") {
		t.Fatalf("provider routes=%v want %v", result.ProviderRoutes, want)
	}
	if result.Added != len(want) || strings.Join(result.AddedModelIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("added=%d ids=%v want %v", result.Added, result.AddedModelIDs, want)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "grok", want[0]); err != nil {
		t.Fatalf("route row missing after the refresh: %v", err)
	}
}

// TestGrokRefreshPublishesProviderRoutesWhenTheBuildCatalogReadFails proves the
// two authorities are independent: a control-plane outage on Build must not leave
// the Web plane unpublished.
func TestGrokRefreshPublishesProviderRoutesWhenTheBuildCatalogReadFails(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	createGrokAccount(t, s, &store.Account{
		Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: grok.ProviderBuild,
		OAuthAccessToken: "access", OAuthRefreshToken: "refresh", Enabled: true,
	})
	createGrokAccount(t, s, &store.Account{
		Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
		ClientCookie: "sso=web-token", Enabled: true,
	})
	previous := fetchGrokBuildModelsForRefresh
	defer func() { fetchGrokBuildModelsForRefresh = previous }()
	fetchGrokBuildModelsForRefresh = func(context.Context, *config.Config, *store.Store, *store.Account) ([]modelcatalog.Profile, error) {
		return nil, errors.New("control plane unavailable")
	}

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 1)
	if err == nil {
		t.Fatalf("refresh result=%+v want the Build read failure reported", result)
	}
	for _, id := range wantedProviderRoutes(grok.UpstreamAppChat) {
		if _, err := s.GetModelByChannelAndModelID(ctx, "grok", id); err != nil {
			t.Fatalf("web route %q missing after the failed Build read: %v", id, err)
		}
	}
}

// TestGrokRefreshCountsProviderRoutesInAdded keeps the admin report honest: one
// refresh that created both observed and route rows must count both.
func TestGrokRefreshCountsProviderRoutesInAdded(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	createGrokAccount(t, s, &store.Account{
		Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: grok.ProviderBuild,
		OAuthAccessToken: "access", OAuthRefreshToken: "refresh", Enabled: true,
	})
	createGrokAccount(t, s, &store.Account{
		Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
		ClientCookie: "sso=web-token", Enabled: true,
	})
	previous := fetchGrokBuildModelsForRefresh
	defer func() { fetchGrokBuildModelsForRefresh = previous }()
	fetchGrokBuildModelsForRefresh = func(context.Context, *config.Config, *store.Store, *store.Account) ([]modelcatalog.Profile, error) {
		return []modelcatalog.Profile{{ModelID: "grok-4.7"}}, nil
	}

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 1)
	if err != nil {
		t.Fatalf("syncModelsForChannelConcurrent() error = %v", err)
	}
	want := wantedProviderRoutes(grok.UpstreamAppChat)
	if len(result.ProviderRoutes) != len(want) {
		t.Fatalf("provider routes=%v want %v", result.ProviderRoutes, want)
	}
	// The Build read publishes grok-4.7 plus the aliases an OAuth account can
	// always serve (Composer), so the created total is asserted against the rows
	// this refresh actually reported rather than against a fixed number.
	if result.Added != len(result.AddedModelIDs) {
		t.Fatalf("added=%d with ids=%v", result.Added, result.AddedModelIDs)
	}
	for _, id := range append([]string{"grok-4.7"}, want...) {
		if !slices.Contains(result.AddedModelIDs, id) {
			t.Fatalf("added ids=%v missing %q", result.AddedModelIDs, id)
		}
	}
}

// TestGrokRefreshOnADeploymentWithEveryPlane mirrors the production shape this
// work came from: Build OAuth accounts plus Web SSO sources and their linked
// Console companions. All three planes must end up routable, and the discovered
// Build row must stay the channel default.
func TestGrokRefreshOnADeploymentWithEveryPlane(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")

	for id := 1; id <= 2; id++ {
		createGrokAccount(t, s, &store.Account{
			Name: "build", AccountType: "grok", CredentialType: "oauth", GrokProvider: grok.ProviderBuild,
			OAuthAccessToken: "access", OAuthRefreshToken: "refresh", Enabled: true,
		})
	}
	for index := 0; index < 5; index++ {
		source := createGrokAccount(t, s, &store.Account{
			Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb,
			ClientCookie: "sso=web-token", Enabled: true,
		})
		createGrokAccount(t, s, &store.Account{
			Name: "web · Console", AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole,
			GrokSSOParentID: source.ID, ClientCookie: "sso=web-token", Enabled: true,
		})
	}
	previous := fetchGrokBuildModelsForRefresh
	defer func() { fetchGrokBuildModelsForRefresh = previous }()
	fetchGrokBuildModelsForRefresh = func(context.Context, *config.Config, *store.Store, *store.Account) ([]modelcatalog.Profile, error) {
		return []modelcatalog.Profile{{ModelID: "grok-4.7"}}, nil
	}

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 2)
	if err != nil {
		t.Fatalf("syncModelsForChannelConcurrent() error = %v", err)
	}
	if result.DefaultModelID != "grok-4.7" {
		t.Fatalf("default model=%q want the discovered Build row", result.DefaultModelID)
	}
	if len(result.ProviderRoutes) != len(wantedProviderRoutes(grok.UpstreamAppChat))+len(wantedProviderRoutes(grok.UpstreamConsole)) {
		t.Fatalf("provider routes=%v", result.ProviderRoutes)
	}
	for _, id := range append(append([]string{"grok-4.7"},
		wantedProviderRoutes(grok.UpstreamAppChat)...), wantedProviderRoutes(grok.UpstreamConsole)...) {
		if _, err := s.GetModelByChannelAndModelID(ctx, "grok", id); err != nil {
			t.Fatalf("route %q missing after the refresh: %v", id, err)
		}
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(model.Channel, "grok") || !model.IsDefault {
			continue
		}
		if !strings.EqualFold(model.Provider, grok.ProviderBuild) {
			t.Fatalf("default row %q is on %s; a route table must not become the default", model.ModelID, model.Provider)
		}
	}
}
