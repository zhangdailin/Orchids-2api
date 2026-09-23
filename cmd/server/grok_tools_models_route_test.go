package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

func TestAdminGrokModelsUsesSessionAuthenticatedPublicCatalog(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "grok_tools_route:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-live", Name: "Grok Live", Status: store.ModelStatusAvailable, Verified: true, Origin: "discovery", Capabilities: []string{store.CapabilityChat}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin"}
	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, grok.NewHandler(cfg, lb), apiHandler, middleware.NewConcurrencyLimiter(4, 0, false), nil, renderer)

	auth := httptest.NewRequest(http.MethodGet, "/api/grok/models", nil)
	auth.Header.Set("X-Admin-Token", cfg.AdminToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" || !containsAll(body, "grok-live", "capabilities") {
		t.Fatalf("body=%s", body)
	}

	unauth := httptest.NewRecorder()
	mux.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/grok/models", nil))
	if unauth.Code == http.StatusOK {
		t.Fatal("catalog allowed without admin session")
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
