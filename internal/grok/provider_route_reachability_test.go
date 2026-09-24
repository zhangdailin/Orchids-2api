package grok

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// publishProviderRoute writes one route row the way the model refresh publishes
// it: the gateway's own route table, verified because the plane has an account.
// It is duplicated here on purpose — the test must fail if the refresh's row
// shape and the routing layer's expectations ever drift apart.
func publishProviderRoute(t *testing.T, ctx context.Context, s *store.Store, modelID string) {
	t.Helper()
	row := &store.Model{
		Channel:   "grok",
		ModelID:   modelID,
		Name:      modelID,
		Status:    store.ModelStatusAvailable,
		SortOrder: 1000,
		Verified:  true,
	}
	store.ApplyGrokRouteDefaults(row)
	if err := s.CreateModel(ctx, row); err != nil {
		t.Fatalf("CreateModel(%s) error = %v", modelID, err)
	}
}

// TestPublishedProviderRoutesReachTheSSOAccounts is the reason the route rows
// exist: without a row, a Web SSO or Console companion account is selected by
// nobody, because every Grok request first has to resolve an enabled model row.
func TestPublishedProviderRoutesReachTheSSOAccounts(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "provider-route:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	ctx := context.Background()

	web := &store.Account{
		Name: "web", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb,
		ClientCookie: "sso=web-token", Subscription: "super", Enabled: true,
	}
	if err := s.CreateAccount(ctx, web); err != nil {
		t.Fatal(err)
	}
	companion := &store.Account{
		Name: "web · Console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole,
		GrokSSOParentID: web.ID, ClientCookie: "sso=console-token", Enabled: true,
	}
	if err := s.CreateAccount(ctx, companion); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(&config.Config{}, loadbalancer.NewWithCacheTTL(s, 0))

	// Before the route is published the model is not servable, even though the
	// compatibility table resolves its name and the account is enabled.
	for _, id := range []string{"grok-chat-auto", "console/grok-4.20-0309-reasoning"} {
		if err := h.ensureModelEnabled(ctx, id); err == nil {
			t.Fatalf("ensureModelEnabled(%q) succeeded before the route row existed", id)
		}
	}

	publishProviderRoute(t, ctx, s, "grok-chat-auto")
	publishProviderRoute(t, ctx, s, "console/grok-4.20-0309-reasoning")

	webSpec, ok := h.resolveConversationModel(ctx, "grok-chat-auto")
	if !ok {
		t.Fatal("grok-chat-auto did not resolve after its route row was published")
	}
	if webSpec.Upstream != UpstreamAppChat || !strings.EqualFold(webSpec.UpstreamModel, "grok-chat-auto") || webSpec.ConsoleModel != "" {
		t.Fatalf("web route = %+v", webSpec)
	}
	webSession, err := h.openChatAccountSessionForModel(ctx, webSpec)
	if err != nil {
		t.Fatalf("openChatAccountSessionForModel() error = %v", err)
	}
	defer webSession.Close()
	if webSession.acc == nil || webSession.acc.ID != web.ID {
		t.Fatalf("web session account = %+v want the Web SSO source", webSession.acc)
	}
	if NormalizeSSOToken(webSession.token) != "web-token" {
		t.Fatalf("web session token = %q", webSession.token)
	}

	consoleSpec, ok := h.resolveConversationModel(ctx, "console/grok-4.20-0309-reasoning")
	if !ok {
		t.Fatal("console/grok-4.20-0309-reasoning did not resolve after its route row was published")
	}
	if consoleSpec.Upstream != UpstreamConsole || consoleSpec.ConsoleModel == "" {
		t.Fatalf("console route = %+v", consoleSpec)
	}
	consoleSession, err := h.openConsoleAccountSession(ctx, nil, "console/grok-4.20-0309-reasoning")
	if err != nil {
		t.Fatalf("openConsoleAccountSession() error = %v", err)
	}
	defer consoleSession.Close()
	if consoleSession.acc == nil || ProviderForAccount(consoleSession.acc) != ProviderConsole {
		t.Fatalf("console session account = %+v want the Console companion", consoleSession.acc)
	}
	if NormalizeSSOToken(consoleSession.token) != "console-token" {
		t.Fatalf("console session token = %q", consoleSession.token)
	}
}

// TestProviderRouteRowsStayOutOfTheDefaultChatModel pins the ordering half of the
// decision: publishing a new plane must not silently move the tools page's
// default chat model, which is the first chat entry in the list.
func TestProviderRouteRowsStayOutOfTheDefaultChatModel(t *testing.T) {
	build := &store.Model{Channel: "grok", ModelID: "grok-4.7", Name: "Grok 4.7", Provider: ProviderBuild, UpstreamModel: "grok-4.7", Status: store.ModelStatusAvailable, Verified: true}
	route := &store.Model{Channel: "grok", ModelID: "grok-chat-auto", Name: "Grok Chat Auto", Status: store.ModelStatusAvailable, Verified: true, SortOrder: 1000}
	store.ApplyGrokRouteDefaults(route)
	if route.SortOrder <= build.SortOrder {
		t.Fatalf("route row sort order %d must follow the discovered rows (%d)", route.SortOrder, build.SortOrder)
	}
}
