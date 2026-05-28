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

	orchidsModel, err := s.GetModelByChannelAndModelID(ctx, "orchids", "claude-opus-4-6")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(orchids) error = %v", err)
	}
	if orchidsModel.Channel != "Orchids" {
		t.Fatalf("orchids model channel = %q, want Orchids", orchidsModel.Channel)
	}

	warpModel, err := s.GetModelByChannelAndModelID(ctx, "warp", "claude-4-5-opus")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(warp) error = %v", err)
	}
	if warpModel.Channel != "Warp" {
		t.Fatalf("warp model channel = %q, want Warp", warpModel.Channel)
	}
	if warpModel.ID == orchidsModel.ID {
		t.Fatalf("expected different records across channels, got same id %q", warpModel.ID)
	}
}

func TestStoreNew_SeedsGrokImagineModels(t *testing.T) {
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
	model, err := s.GetModelByChannelAndModelID(ctx, "grok", "grok-imagine-image")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(grok, grok-imagine-image) error = %v", err)
	}
	if model == nil {
		t.Fatal("expected grok imagine model to be seeded")
	}
	if model.Channel != "Grok" {
		t.Fatalf("model.Channel=%q want %q", model.Channel, "Grok")
	}
	if model.Status != ModelStatusAvailable {
		t.Fatalf("model.Status=%q want %q", model.Status, ModelStatusAvailable)
	}
}
