package store

import (
	"context"
	"github.com/goccy/go-json"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestModelStatus_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    ModelStatus
		enabled bool
	}{
		{name: "bool true", input: `true`, want: ModelStatusAvailable, enabled: true},
		{name: "bool false", input: `false`, want: ModelStatusOffline, enabled: false},
		{name: "available", input: `"available"`, want: ModelStatusAvailable, enabled: true},
		{name: "maintenance", input: `"maintenance"`, want: ModelStatusMaintenance, enabled: false},
		{name: "offline", input: `"offline"`, want: ModelStatusOffline, enabled: false},
		{name: "unknown", input: `"something"`, want: ModelStatusOffline, enabled: false},
		{name: "null", input: `null`, want: ModelStatusOffline, enabled: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s ModelStatus
			if err := json.Unmarshal([]byte(tt.input), &s); err != nil {
				t.Fatalf("unmarshal failed: %v", err)
			}
			if s != tt.want {
				t.Fatalf("got %q want %q", s, tt.want)
			}
			if s.Enabled() != tt.enabled {
				t.Fatalf("enabled=%v want %v", s.Enabled(), tt.enabled)
			}
		})
	}
}

func TestModelStatus_MarshalJSON(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(ModelStatusAvailable)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(b) != `"available"` {
		t.Fatalf("got %s want %s", string(b), `"available"`)
	}
}

func TestGetModelByChannelAndModelID_AllowsDuplicateModelIDsAcrossChannels(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	ctx := context.Background()

	// The store publishes nothing on its own, so the two channels' fixtures are
	// created explicitly. The point of the test is that the lookup index is keyed
	// by channel *and* model id, not that either channel has a catalog.
	if err := s.CreateModel(ctx, &Model{
		Channel: "Puter", ModelID: "deepseek-v4-pro", Name: "deepseek-v4-pro",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(puter) error = %v", err)
	}
	if err := s.CreateModel(ctx, &Model{
		Channel: "Warp", ModelID: "auto-open", Name: "Warp Auto Open",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(warp) error = %v", err)
	}

	puterModel, err := s.GetModelByChannelAndModelID(ctx, "puter", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(puter) error = %v", err)
	}
	if puterModel.Channel != "Puter" {
		t.Fatalf("puter model channel = %q, want Puter", puterModel.Channel)
	}

	warpModel, err := s.GetModelByChannelAndModelID(ctx, "warp", "auto-open")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(warp) error = %v", err)
	}
	if warpModel.Channel != "Warp" {
		t.Fatalf("warp model channel = %q, want Warp", warpModel.Channel)
	}
	if warpModel.ID == puterModel.ID {
		t.Fatalf("expected different records across channels, got same id %q", warpModel.ID)
	}
}

// TestStoreNew_PublishesNoBuiltInModels pins the startup contract: a new store
// carries no model rows at all.
//
// Model management publishes only catalogs read from upstream for an active
// account, so a fresh deployment starts empty. A compiled-in seed here would
// make the admin page report models no account ever advertised, and would keep
// them served after an upstream withdrew them.
func TestStoreNew_PublishesNoBuiltInModels(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	ctx := context.Background()
	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ListModels() = %d rows, want none: %+v", len(models), models)
	}
	for _, probe := range []struct{ channel, modelID string }{
		{"Grok", "grok-4.5"},
		{"Grok", "grok-imagine-image"},
		{"Warp", "auto-open"},
		{"Puter", "claude-opus-5"},
		{"WorkBuddy", "default-model"},
		{"Qoder", "Qwen3.7-Max"},
	} {
		if _, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID); err == nil {
			t.Fatalf("%s/%s was published at startup, want an empty catalog", probe.channel, probe.modelID)
		}
	}
}

// TestStoreNew_PreservesExistingModelList proves a restart does not resurrect a
// deleted row: nothing recreates model records at startup.
func TestStoreNew_PreservesExistingModelList(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}

	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{
		Channel: "Puter", ModelID: "deepseek-v4-pro", Name: "deepseek-v4-pro",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	model, err := s.GetModelByChannelAndModelID(ctx, "puter", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if err := s.DeleteModel(ctx, model.ID); err != nil {
		t.Fatalf("DeleteModel() error = %v", err)
	}
	_ = s.Close()

	s, err = New(opts)
	if err != nil {
		t.Fatalf("store.New() second error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	if _, err := s.GetModelByChannelAndModelID(ctx, "puter", "deepseek-v4-pro"); err == nil {
		t.Fatal("expected deleted model to stay deleted after store restart")
	}
}

// TestStoreNew_KeepsUpstreamDiscoveredModels proves startup maintenance never
// prunes a row that an upstream refresh published. Pruning is the refresh's job:
// it knows which catalog it just read, while startup knows nothing.
func TestStoreNew_KeepsUpstreamDiscoveredModels(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{
		Channel: "Puter", ModelID: "claude-opus-5", Name: "claude-opus-5",
		Status: ModelStatusAvailable, Verified: true, Origin: "discovery",
	}); err != nil {
		t.Fatalf("CreateModel(discovered) error = %v", err)
	}
	if err := s.CreateModel(ctx, &Model{
		Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6",
		Status: ModelStatusAvailable, Verified: true, Origin: "discovery",
	}); err != nil {
		t.Fatalf("CreateModel(grok discovered) error = %v", err)
	}
	_ = s.Close()

	s, err = New(opts)
	if err != nil {
		t.Fatalf("store.New() second error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	for _, probe := range []struct{ channel, modelID string }{
		{"Puter", "claude-opus-5"},
		{"Grok", "grok-4.6"},
	} {
		model, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID)
		if err != nil || model == nil {
			t.Fatalf("discovered model %s/%s did not survive a restart: %v", probe.channel, probe.modelID, err)
		}
		if !model.Verified {
			t.Fatalf("%s/%s lost its verified flag", probe.channel, probe.modelID)
		}
	}
}

// TestStoreNew_RemovesDeprecatedGrokModelsOnly proves startup cleanup is limited
// to identifiers known to be dead. Nothing is added, and a verified row that is
// not on the deprecated list survives untouched.
func TestStoreNew_RemovesDeprecatedGrokModelsOnly(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}

	ctx := context.Background()
	for _, record := range []*Model{
		{Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Grok", ModelID: "grok-imagine-image-quality", Name: "Grok Imagine Image Quality", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Grok", ModelID: "grok-4.3", Name: "legacy console model", Status: ModelStatusAvailable, Verified: true},
		{Channel: "Grok", ModelID: "grok-user-custom", Name: "User Custom", Status: ModelStatusAvailable, Verified: true},
	} {
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel(%s) error = %v", record.ModelID, err)
		}
	}
	_ = s.Close()

	s, err = New(opts)
	if err != nil {
		t.Fatalf("store.New() second error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	for _, id := range []string{"grok-4.5", "grok-imagine-image-quality", "grok-user-custom"} {
		if _, err := s.GetModelByChannelAndModelID(ctx, "grok", id); err != nil {
			t.Fatalf("expected %s to survive startup cleanup: %v", id, err)
		}
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "grok", "grok-4.3"); err == nil {
		t.Fatal("expected deprecated grok-4.3 to be removed")
	}
}

// TestCleanupDeprecatedModelIDsIsChannelScoped proves a retired identifier is
// only removed from the channel that retired it.
//
// The cleanup used to match by identifier alone, which deleted working models:
// the Puter and Warp upstream catalogs legitimately advertise grok-4.3 and
// grok-build-0.1, so every restart removed rows a refresh had just published.
func TestCleanupDeprecatedModelIDsIsChannelScoped(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	s, err := New(Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	ctx := context.Background()
	for _, record := range []*Model{
		{Channel: "Grok", ModelID: "grok-4.3", Name: "retired Grok route", Status: ModelStatusAvailable, Verified: true},
		{Channel: "Puter", ModelID: "grok-4.3", Name: "upstream Puter route", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Warp", ModelID: "grok-build-0.1", Name: "upstream Warp route", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Grok", ModelID: "grok-build-0.1", Name: "retired Grok route", Status: ModelStatusAvailable, Verified: true},
		{Channel: "Warp", ModelID: "warp-chat", Name: "retired virtual mode", Status: ModelStatusAvailable, Verified: true},
	} {
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel(%s/%s) error = %v", record.Channel, record.ModelID, err)
		}
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})

	s.cleanupDeprecatedModelIDs(ctx)

	for _, probe := range []struct{ channel, modelID string }{
		{"Puter", "grok-4.3"},
		{"Warp", "grok-build-0.1"},
	} {
		if _, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID); err != nil {
			t.Fatalf("%s/%s was deleted from a channel that did not retire it: %v", probe.channel, probe.modelID, err)
		}
	}
	for _, probe := range []struct{ channel, modelID string }{
		{"Grok", "grok-4.3"},
		{"Grok", "grok-build-0.1"},
		{"Warp", "warp-chat"},
	} {
		if _, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID); err == nil {
			t.Fatalf("%s/%s was not retired", probe.channel, probe.modelID)
		}
	}
}
