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

func TestAdminGrokToolsRoutesUseSessionWithoutClientKey(t *testing.T) {
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

	const modelsPath = "/api/grok/tools/v1/models"
	unauth := httptest.NewRecorder()
	mux.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, modelsPath, nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d body=%s", unauth.Code, unauth.Body.String())
	}

	auth := httptest.NewRequest(http.MethodGet, modelsPath, nil)
	auth.Header.Set("X-Admin-Token", cfg.AdminToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("session-auth status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" || !containsAll(body, "grok-live", "capabilities") {
		t.Fatalf("body=%s", body)
	}
	if strings.Contains(rec.Body.String(), "Missing API key") {
		t.Fatalf("admin tools route incorrectly used client inference auth: %s", rec.Body.String())
	}

	public := httptest.NewRecorder()
	mux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/grok/v1/models", nil))
	if public.Code != http.StatusUnauthorized || !strings.Contains(public.Body.String(), "Missing API key") {
		t.Fatalf("public Grok route must retain key auth: status=%d body=%s", public.Code, public.Body.String())
	}

	// Every requested tool endpoint must be protected by the admin session and
	// must not fall through to an unprotected not-found handler. Handler behavior
	// is tested rather than ServeMux pattern names because registerRoutes mounts
	// its private mux behind the root dispatcher.
	paths := []string{
		"/api/grok/tools/v1/models/grok-live",
		"/api/grok/tools/v1/responses", "/api/grok/tools/v1/responses/compact", "/api/grok/tools/v1/responses/resp_1",
	}
	for _, path := range paths {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated status=%d, want 401", path, rec.Code)
		}
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
