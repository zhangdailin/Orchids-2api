package main

import (
	"encoding/json"
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

// TestRegisterRoutes_WorkBuddyEndpoints proves the WorkBuddy inference channel
// and the official browser login are reachable through the real route table and
// stay behind their respective auth layers.
func TestRegisterRoutes_WorkBuddyEndpoints(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "routes:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin"}
	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	t.Cleanup(h.Close)
	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatalf("template.NewRenderer() error = %v", err)
	}
	limiter := middleware.NewConcurrencyLimiter(4, 0, false)

	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, apiHandler, limiter, nil, renderer)

	// The login endpoints must not be usable without an admin session.
	unauth := httptest.NewRecorder()
	mux.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/workbuddy/login", strings.NewReader("{}")))
	if unauth.Code != http.StatusUnauthorized && unauth.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated login status = %d, want 401/403", unauth.Code)
	}

	// Authorized requests reach the handler, which asks the upstream for a fresh
	// authorization transaction. localhost is used so the same-origin HTTPS
	// guard accepts the plain-HTTP test request.
	req := httptest.NewRequest(http.MethodPost, "/api/workbuddy/login", strings.NewReader("{}"))
	req.Header.Set("X-Admin-Token", "admintoken")
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("Content-Type", "application/json")
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized login status = %d body=%s", rec.Code, rec.Body.String())
	}
	var started struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode login response: %v (body=%s)", err, rec.Body.String())
	}
	if started.ID == "" || started.Status != "pending" {
		t.Fatalf("login response = %+v", started)
	}
	if !strings.HasPrefix(started.VerificationURIComplete, "https://www.workbuddy.ai/login?") {
		t.Fatalf("login response is not the official login page: %q", started.VerificationURIComplete)
	}

	// Poll the transaction, then cancel it so the test leaves no pending login.
	pollReq := httptest.NewRequest(http.MethodGet, "/api/workbuddy/login/"+started.ID, nil)
	pollReq.Header.Set("X-Admin-Token", "admintoken")
	pollRec := httptest.NewRecorder()
	mux.ServeHTTP(pollRec, pollReq)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d body=%s", pollRec.Code, pollRec.Body.String())
	}
	cancelReq := httptest.NewRequest(http.MethodDelete, "/api/workbuddy/login/"+started.ID, nil)
	cancelReq.Header.Set("X-Admin-Token", "admintoken")
	cancelRec := httptest.NewRecorder()
	mux.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", cancelRec.Code)
	}

	// The WorkBuddy channel entry points are routed for both protocols. Without
	// an API key the inference auth layer rejects the request, which still proves
	// the path is registered rather than unknown.
	for _, target := range []string{"/workbuddy/v1/messages", "/workbuddy/v1/chat/completions", "/workbuddy/v1/models"} {
		channelReq := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"hy3","messages":[]}`))
		channelRec := httptest.NewRecorder()
		mux.ServeHTTP(channelRec, channelReq)
		if channelRec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s rejected the method, path is wrong", target)
		}
		if strings.Contains(channelRec.Body.String(), "404 page not found") {
			t.Fatalf("%s is not routed", target)
		}
	}
}
