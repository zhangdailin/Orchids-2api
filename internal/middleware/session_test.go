package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionAuth_AdminPassBearer(t *testing.T) {
	called := false
	handler := SessionAuth("admin123", "", func(w http.ResponseWriter, r *http.Request) {
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
	handler := SessionAuth("admin123", "admintoken", func(w http.ResponseWriter, r *http.Request) {
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
	handler := SessionAuth("admin123", "admintoken", func(w http.ResponseWriter, r *http.Request) {
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
	handler := SessionAuth("admin123", "admintoken", func(w http.ResponseWriter, r *http.Request) {
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

func TestPublicKeyAuth_ValidBearer(t *testing.T) {
	called := false
	handler := PublicKeyAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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
	handler := PublicKeyAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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

func TestPublicKeyAuth_AllowsWhenNoKeyAndDisabled(t *testing.T) {
	called := false
	handler := PublicKeyAuth("", false, func(w http.ResponseWriter, r *http.Request) {
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

func TestPublicKeyAuth_EnabledWhenNoKey(t *testing.T) {
	called := false
	handler := PublicKeyAuth("", true, func(w http.ResponseWriter, r *http.Request) {
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
	handler := PublicImagineStreamAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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
	handler := PublicImagineStreamAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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
	handler2 := PublicImagineStreamAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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
	handler3 := PublicImagineStreamAuth("pub-123", false, func(w http.ResponseWriter, r *http.Request) {
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
	handler := PublicImagineStreamAuth("", false, func(w http.ResponseWriter, r *http.Request) {
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
