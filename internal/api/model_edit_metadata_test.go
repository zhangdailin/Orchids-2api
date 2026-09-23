package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestModelAdminEditPreservesDiscoveredRoutingMetadata(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "model_edit:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	model := &store.Model{Channel: "Grok", ModelID: "grok-4.7", Name: "old", Status: store.ModelStatusAvailable, Verified: true, Provider: "build", UpstreamModel: "grok-4.7", Capabilities: []string{store.CapabilityChat, store.CapabilityResponses}, Origin: "discovery", BoundAccountIDs: []int64{7}, CreatedAt: time.Now().UTC()}
	if err := s.CreateModel(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	a := New(s, "admin", "pass", &config.Config{})
	req := httptest.NewRequest(http.MethodPut, "/api/models/"+model.ID, strings.NewReader(`{"channel":"Grok","model_id":"grok-4.7","name":"renamed","status":"maintenance","sort_order":9,"is_default":true}`))
	rec := httptest.NewRecorder()
	a.HandleModelByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := s.GetModel(context.Background(), model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Verified || got.Provider != "build" || got.UpstreamModel != "grok-4.7" || got.Origin != "discovery" || len(got.Capabilities) != 2 || len(got.BoundAccountIDs) != 1 || got.CreatedAt.IsZero() {
		t.Fatalf("metadata was lost: %+v", got)
	}
	if got.Name != "renamed" || got.Status != store.ModelStatusMaintenance || !got.IsDefault || got.SortOrder != 9 {
		t.Fatalf("editable fields not applied: %+v", got)
	}
}
