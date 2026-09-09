package store

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestGrokVideoRouteDefaults(t *testing.T) {
	for _, tc := range []struct{ id, provider, upstream string }{
		{"grok-imagine-video", "web", "imagine-video-gen"},
		{"grok-imagine-video-1.5", "console", "grok-imagine-video-1.5"},
		{"build/grok-imagine-video-1.5", "build", "grok-imagine-video-1.5"},
	} {
		m := Model{ModelID: tc.id}
		applyGrokRouteDefaults(&m)
		if m.Provider != tc.provider || m.UpstreamModel != tc.upstream {
			t.Fatalf("%s route = %s/%s", tc.id, m.Provider, m.UpstreamModel)
		}
	}
}

func TestBackfillGrokVideoRoutePreservesOverrides(t *testing.T) {
	for _, tc := range []struct{ name, provider, upstream, want string }{
		{"legacy", "web", "grok-imagine-video", "imagine-video-gen"},
		{"correct", "web", "imagine-video-gen", "imagine-video-gen"},
		{"custom", "web", "custom-video", "custom-video"},
		{"console", "console", "grok-imagine-video", "grok-imagine-video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mini := miniredis.RunT(t)
			s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "video-route-test:"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()
			m := &Model{Channel: "Grok", ModelID: "grok-imagine-video", Name: "Video", Provider: tc.provider, UpstreamModel: tc.upstream, Capabilities: []string{CapabilityVideo}, Status: ModelStatusAvailable}
			if err := s.CreateModel(ctx, m); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				s.backfillGrokRouteMetadata(ctx)
				got, err := s.GetModelByChannelAndModelID(ctx, "grok", m.ModelID)
				if err != nil {
					t.Fatal(err)
				}
				if got.UpstreamModel != tc.want || got.Provider != tc.provider {
					t.Fatalf("route = %#v", got)
				}
			}
		})
	}
}
