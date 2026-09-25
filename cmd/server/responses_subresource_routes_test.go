package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

// TestRegisterRoutes_ResponsesSubResources proves the endpoints below a response
// id are reachable through the real route table on every prefix.
//
// They are registered explicitly rather than through the model dispatcher
// because a cancel body carries no model: dispatching them by body would send
// the same request to the native handler or the bridge depending on whether the
// client sent `{}` or nothing. A 404 page-not-found here would mean the route is
// missing; the response_not_found envelope means the route exists and the store
// simply has no such record.
func TestRegisterRoutes_ResponsesSubResources(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "responses-routes:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Inference auth is unconditional (grok2api has no switch on /v1), so the
	// probe has to carry a managed key to reach the routing layer.
	cfg := &config.Config{
		AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
	}
	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	t.Cleanup(h.Close)
	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatalf("template.NewRenderer() error = %v", err)
	}
	limiter := middleware.NewConcurrencyLimiter(4, 0, false)

	// A managed key, because /v1 requires one unconditionally.
	managedKey := "sk-responses-subresource"
	digest := sha256.Sum256([]byte(managedKey))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{
		Name: "subresource-probe", KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "urce", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateApiKey() error = %v", err)
	}

	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, apiHandler, limiter, nil, renderer)

	for _, prefix := range []string{"/v1", "/grok/v1", "/warp/v1", "/workbuddy/v1", "/qoder/v1"} {
		t.Run(prefix, func(t *testing.T) {
			for _, probe := range []struct {
				method string
				path   string
				body   string
			}{
				{http.MethodPost, prefix + "/responses/resp_absent/cancel", "{}"},
				{http.MethodPost, prefix + "/responses/resp_absent/cancel", ""},
				{http.MethodGet, prefix + "/responses/resp_absent/input_items", ""},
			} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(probe.method, probe.path, strings.NewReader(probe.body))
				req.Header.Set("Authorization", "Bearer "+managedKey)
				mux.ServeHTTP(rec, req)

				if rec.Code == http.StatusMethodNotAllowed {
					t.Fatalf("%s %s returned 405: the path is registered for the wrong method", probe.method, probe.path)
				}
				if strings.Contains(rec.Body.String(), "404 page not found") {
					t.Fatalf("%s %s is not routed", probe.method, probe.path)
				}
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s %s status = %d, want 404 (body=%s)", probe.method, probe.path, rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), "response_not_found") {
					t.Fatalf("%s %s body = %s, want the response_not_found envelope", probe.method, probe.path, rec.Body.String())
				}
			}

			// A wrong method must still be answered by the handler (405 with an
			// Allow header), which is the difference between "this endpoint
			// rejects GET" and "this endpoint does not exist".
			rec := httptest.NewRecorder()
			wrongMethod := httptest.NewRequest(http.MethodGet, prefix+"/responses/resp_absent/cancel", nil)
			wrongMethod.Header.Set("Authorization", "Bearer "+managedKey)
			mux.ServeHTTP(rec, wrongMethod)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("GET %s/responses/resp_absent/cancel status = %d, want 405", prefix, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
				t.Fatalf("Allow = %q, want POST", allow)
			}
		})
	}

	// The response id itself must keep working on the unified prefix: the
	// explicit sibling routes must not shadow the /responses/ subtree.
	rec := httptest.NewRecorder()
	resourceReq := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_absent", nil)
	resourceReq.Header.Set("Authorization", "Bearer "+managedKey)
	mux.ServeHTTP(rec, resourceReq)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "response_not_found") {
		t.Fatalf("GET /v1/responses/resp_absent status = %d body = %s", rec.Code, rec.Body.String())
	}
}
