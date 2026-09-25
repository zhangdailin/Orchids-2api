package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

// TestHealthReportsEveryRemainingProvider pins /health's body: it answers "ok"
// and lists one entry per registered channel, so the probe can no longer report a
// provider this gateway does not serve. The set is asserted against the channel
// registry rather than a literal, which is what makes a removed provider
// disappear from the response automatically.
func TestHealthReportsEveryRemainingProvider(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "health:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminPath: "/admin"}
	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	t.Cleanup(h.Close)
	a := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, a, middleware.NewConcurrencyLimiter(4, 0, false), nil, renderer)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health body: %v (%s)", err, rec.Body.String())
	}
	if body.Status != "ok" {
		t.Fatalf("status=%q, want ok", body.Status)
	}
	want := map[string]struct{}{}
	for _, definition := range channel.All() {
		want[string(definition.ID)] = struct{}{}
		if got := body.Providers[string(definition.ID)]; got != "ready" {
			t.Fatalf("providers[%s]=%q, want ready", definition.ID, got)
		}
	}
	if len(body.Providers) != len(want) {
		t.Fatalf("providers=%v, want exactly the registered channels %v", body.Providers, want)
	}
}
