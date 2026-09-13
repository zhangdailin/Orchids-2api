package main

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/alicebob/miniredis/v2"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"testing"
)

func TestV1RoutesFailClosedAndChargeOnce(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "secure-routes:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	disabled := false
	cfg := &config.Config{AdminUser: "admin", AdminPass: "test-secret", AdminPath: "/admin", InferenceAuth: &disabled}
	h := handler.NewWithLoadBalancer(cfg, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()
	a := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, a, middleware.NewConcurrencyLimiter(4, 0, false), nil, renderer)
	for _, path := range []string{"/v1/models", "/v1/responses", "/v1/files/test", "/v1/media/uploads/old", "/v1/public/imagine/config", "/v1/admin/verify", "/v1/unknown", "/warp/v1/models", "/puter/v1/models", "/workbuddy/v1/models", "/grok/v1/files/test"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("x-api-key", "test-key")
		r.Header.Set("X-Admin-Token", "test-secret")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != 401 {
			t.Fatalf("%s allowed non-Bearer auth: %d", path, rec.Code)
		}
	}
	digest := sha256.Sum256([]byte("managed-test-key"))
	key := &store.ApiKey{Name: "test", KeyHash: hex.EncodeToString(digest[:]), Enabled: true, RPMLimit: 1, MaxConcurrent: 1}
	if err := s.CreateApiKey(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{200, 429} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer managed-test-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != status {
			t.Fatalf("one request charged more than once: got %d want %d", rec.Code, status)
		}
	}
}
