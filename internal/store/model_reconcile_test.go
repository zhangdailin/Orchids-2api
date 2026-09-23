package store

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestReconcileDiscoveredModelsProtectsManualAndPrunesDiscovery(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); mini.Close() })
	ctx := context.Background()
	manual := &Model{Channel: "Qoder", ModelID: "manual", Name: "operator name", Status: ModelStatusMaintenance}
	stale := &Model{Channel: "Qoder", ModelID: "stale", Name: "stale", Status: ModelStatusAvailable, Origin: "discovery"}
	other := &Model{Channel: "Puter", ModelID: "stale", Name: "other channel", Status: ModelStatusAvailable, Origin: "discovery"}
	for _, m := range []*Model{manual, stale, other} {
		if err := s.CreateModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	result, err := s.ReconcileDiscoveredModels(ctx, " Qoder ", []*Model{
		{ModelID: "manual", Name: "feed name", Status: ModelStatusAvailable},
		{ModelID: "fresh", Name: "Fresh", Status: ModelStatusAvailable, Verified: true},
	}, ModelReconcileOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 || result.Deleted != 1 || result.Protected != 1 {
		t.Fatalf("result=%+v", result)
	}
	gotManual, err := s.GetModelByChannelAndModelID(ctx, "qoder", "manual")
	if err != nil || gotManual.Name != "operator name" || gotManual.Status != ModelStatusMaintenance || gotManual.Origin != "manual" {
		t.Fatalf("manual=%+v err=%v", gotManual, err)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "qoder", "stale"); err == nil {
		t.Fatal("stale discovery row survived prune")
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "puter", "stale"); err != nil {
		t.Fatalf("other channel pruned: %v", err)
	}
	fresh, err := s.GetModelByChannelAndModelID(ctx, "qoder", "fresh")
	if err != nil || fresh.Origin != "discovery" || fresh.Channel != "Qoder" {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
}

func TestReconcileDiscoveredModelsUpsertsAndOptionalPrune(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); mini.Close() })
	ctx := context.Background()
	old := &Model{Channel: "Cline", ModelID: "same", Name: "old", Status: ModelStatusOffline, Origin: "discovery"}
	missing := &Model{Channel: "Cline", ModelID: "missing", Name: "missing", Status: ModelStatusAvailable, Origin: "discovery"}
	for _, m := range []*Model{old, missing} {
		if err := s.CreateModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	oldID := old.ID

	result, err := s.ReconcileDiscoveredModels(ctx, "Cline", []*Model{{ModelID: "same", Name: "new", Status: ModelStatusAvailable}}, ModelReconcileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Deleted != 0 {
		t.Fatalf("result=%+v", result)
	}
	updated, err := s.GetModelByChannelAndModelID(ctx, "cline", "same")
	if err != nil || updated.ID != oldID || updated.Name != "new" || updated.Origin != "discovery" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "cline", "missing"); err != nil {
		t.Fatalf("non-prune removed missing: %v", err)
	}
}

func TestReconcileDiscoveredModelsValidatesBeforeWriting(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "test:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(); mini.Close() })
	ctx := context.Background()
	_, err = s.ReconcileDiscoveredModels(ctx, "Warp", []*Model{{ModelID: "dup"}, {ModelID: "dup"}}, ModelReconcileOptions{Prune: true})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	models, err := s.ListModels(ctx)
	if err != nil || len(models) != 0 {
		t.Fatalf("partial write: models=%+v err=%v", models, err)
	}
}
