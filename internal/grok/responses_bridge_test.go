package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// responsesBridgeFixture builds a handler whose Build upstream answers with the
// supplied status, plus a verified model record so the Build catalog gate
// lets the request through (exactly as a real model refresh leaves it).
func responsesBridgeFixture(t *testing.T, upstreamStatus int, upstreamBody, model string) *Handler {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(upstream.Close)

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "bridge:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:    "grok",
		CredentialType: "oauth", OAuthAccessToken: "build-token",
		Enabled:      true,
		Subscription: "super",
		Weight:       1,
		AgentMode:    model,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := s.CreateModel(ctx, &store.Model{
		Channel:       "grok",
		ModelID:       model,
		Name:          model,
		Status:        store.ModelStatusAvailable,
		Verified:      true,
		Provider:      ProviderBuild,
		UpstreamModel: model,
		Origin:        "discovery",
		Capabilities:  []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses},
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	return NewHandler(&config.Config{GrokCLIBaseURL: upstream.URL}, loadbalancer.NewWithCacheTTL(s, 0))
}

// TestHandleResponses_PreservesInboundAuthHeaders guards the internal
// Responses -> Chat bridge: rebuilding the header set used to drop the caller's
// Authorization / x-api-key, which is the credential that authorized the request
// and the identity downstream code expects to observe.
// TestHandleResponses_RelaysUpstreamFailureStatus pins the observable contract:
// an upstream failure must surface with the real status and body, not as a bare
// 500 with no explanation.
func TestHandleResponses_RelaysUpstreamFailureStatus(t *testing.T) {
	h := responsesBridgeFixture(t, http.StatusUnauthorized,
		`{"error":{"message":"Grok Build upstream says the OAuth token is invalid"}}`, "grok-4.6")

	body := `{"model":"grok-4.6","input":"hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.HandleResponses(rec, req)

	payload, _ := io.ReadAll(rec.Result().Body)
	t.Logf("status=%d body=%s", rec.Code, strings.TrimSpace(string(payload)))
	if rec.Code == http.StatusOK {
		t.Fatal("an upstream 401 must not be reported as success")
	}
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("upstream failure reported as a bare 500: %s", payload)
	}
	if !strings.Contains(string(payload), "upstream") {
		t.Fatalf("upstream failure body was not relayed: %s", payload)
	}
}

// TestHandleResponses_ForbiddenModelIsNotServerError keeps a model-permission
// problem a 4xx for the Responses endpoint as well, matching chat completions.
func TestHandleResponses_ForbiddenModelIsNotServerError(t *testing.T) {
	h := responsesBridgeFixture(t, http.StatusOK, `{}`, "grok-4.6")

	body := `{"model":"grok-4.6","input":"hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// A key restricted to another model must be refused with 403, never 500.
	rec := httptest.NewRecorder()

	wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 1, AllowedModels: []string{"grok-4.5"}}, nil
	}, h.HandleResponses)
	req.Header.Set("Authorization", "Bearer test-key")
	wrapped(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
}
