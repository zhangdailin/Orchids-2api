package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyAuth(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		token      string
		valid      bool
		validate   error
		wantCode   int
		wantCalled bool
	}{
		{name: "disabled", enabled: false, wantCode: http.StatusNoContent, wantCalled: true},
		{name: "missing", enabled: true, wantCode: http.StatusUnauthorized},
		{name: "invalid", enabled: true, token: "bad", wantCode: http.StatusUnauthorized},
		{name: "backend unavailable", enabled: true, token: "key", validate: errors.New("redis down"), wantCode: http.StatusServiceUnavailable},
		{name: "valid", enabled: true, token: "key", valid: true, wantCode: http.StatusNoContent, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := APIKeyAuthWithRequest(func(*http.Request) bool { return tt.enabled }, func(_ context.Context, token string) (*APIKeyPrincipal, error) {
				if token != tt.token {
					t.Fatalf("token=%q want=%q", token, tt.token)
				}
				if !tt.valid {
					return nil, tt.validate
				}
				return &APIKeyPrincipal{}, tt.validate
			}, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tt.wantCode || called != tt.wantCalled {
				t.Fatalf("status=%d called=%v want status=%d called=%v", rec.Code, called, tt.wantCode, tt.wantCalled)
			}
		})
	}
}

func TestAPIKeyAuthAcceptsAnthropicHeader(t *testing.T) {
	called := false
	h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(_ context.Context, token string) (*APIKeyPrincipal, error) {
		if token == "anthropic-key" {
			return &APIKeyPrincipal{}, nil
		}
		return nil, nil
	}, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/messages", nil)
	req.Header.Set("x-api-key", "anthropic-key")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("status=%d called=%v", rec.Code, called)
	}
}

func TestAPIKeyAuthAddsNonSecretFingerprint(t *testing.T) {
	const token = "sk-sensitive"
	h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(_ context.Context, got string) (*APIKeyPrincipal, error) {
		if got == token {
			return &APIKeyPrincipal{}, nil
		}
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		fingerprint := APIKeyFingerprint(r.Context())
		if fingerprint == "" || strings.Contains(fingerprint, token) {
			t.Fatalf("unsafe fingerprint %q", fingerprint)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestAPIKeyAuthEnforcesDenialsAndModelPolicy(t *testing.T) {
	tests := []struct {
		name      string
		principal *APIKeyPrincipal
		wantCode  int
		wantRetry bool
	}{
		{name: "expired", principal: &APIKeyPrincipal{DenialCode: APIKeyDenialExpired}, wantCode: http.StatusUnauthorized},
		{name: "rate limited", principal: &APIKeyPrincipal{DenialCode: APIKeyDenialRateLimited}, wantCode: http.StatusTooManyRequests, wantRetry: true},
		{name: "model denied", principal: &APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, wantCode: http.StatusForbidden},
		{name: "model allowed case insensitive", principal: &APIKeyPrincipal{AllowedModels: []string{"GROK-4.5"}}, wantCode: http.StatusNoContent},
		{name: "wildcard", principal: &APIKeyPrincipal{AllowedModels: []string{"*"}}, wantCode: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*APIKeyPrincipal, error) {
				return tt.principal, nil
			}, func(w http.ResponseWriter, r *http.Request) {
				if !APIKeyAllowsModel(r.Context(), "grok-4.5") {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer key")
			rec := httptest.NewRecorder()
			h(rec, req)
			if rec.Code != tt.wantCode {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if (rec.Header().Get("Retry-After") != "") != tt.wantRetry {
				t.Fatalf("Retry-After=%q", rec.Header().Get("Retry-After"))
			}
		})
	}
}

func TestSessionAuth_AdminPassBearer(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer admin123")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestSessionAuth_QueryAppKey(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/batch/task/stream?app_key=admin123", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestSessionAuth_QueryPublicKey(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/video/sse?public_key=admin123", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestSessionAuth_Unauthorized(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if called {
		t.Fatalf("expected handler not to be called")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSessionAuthDynamic_UsesLatestCredentials(t *testing.T) {
	called := false
	adminPass := "old-pass"
	handler := SessionAuthDynamic(func() (string, string) {
		return adminPass, ""
	}, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	adminPass = "new-pass"
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer new-pass")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestPublicKeyAuth_ValidBearer(t *testing.T) {
	called := false
	handler := PublicKeyAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/verify", nil)
	req.Header.Set("Authorization", "Bearer pub-123")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestPublicKeyAuth_MissingBearer(t *testing.T) {
	handler := PublicKeyAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/verify", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate=%q want=Bearer", got)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if !strings.Contains(rec.Body.String(), "Missing authentication token") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestPublicKeyAuth_EmptyKeyAllowsEveryRequest(t *testing.T) {
	called := false
	handler := PublicKeyAuth("", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/verify", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestPublicImagineStreamAuth_AllowsTaskIDWithoutKey(t *testing.T) {
	called := false
	handler := PublicImagineStreamAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/sse?task_id=task-1", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}

func TestPublicImagineStreamAuth_RequiresQueryPublicKey(t *testing.T) {
	handler := PublicImagineStreamAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/sse", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(rec.Body.String(), "Missing authentication token") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/sse?public_key=pub-123", nil)
	rec2 := httptest.NewRecorder()
	called := false
	handler2 := PublicImagineStreamAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler2(rec2, req2)
	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec2.Code, http.StatusOK)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/sse", nil)
	req3.Header.Set("Authorization", "Bearer pub-123")
	rec3 := httptest.NewRecorder()
	calledBearer := false
	handler3 := PublicImagineStreamAuth("pub-123", func(w http.ResponseWriter, r *http.Request) {
		calledBearer = true
		w.WriteHeader(http.StatusOK)
	})
	handler3(rec3, req3)
	if !calledBearer {
		t.Fatalf("expected bearer auth to pass")
	}
	if rec3.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec3.Code, http.StatusOK)
	}
}

func TestPublicImagineStreamAuth_AllowsWhenNoKey(t *testing.T) {
	called := false
	handler := PublicImagineStreamAuth("", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/public/imagine/sse", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}
