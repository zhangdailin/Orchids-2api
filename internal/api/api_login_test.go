package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleLogin_SecureCookieDependsOnHTTPS(t *testing.T) {
	a := &API{adminUser: "admin", adminPass: "pass"}

	body := []byte(`{"username":"admin","password":"pass"}`)

	// HTTP: should not set Secure
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", bytes.NewReader(body))
		a.HandleLogin(rec, req)
		set := rec.Header().Get("Set-Cookie")
		if set == "" {
			t.Fatalf("expected Set-Cookie")
		}
		if strings.Contains(strings.ToLower(set), "secure") {
			t.Fatalf("expected no Secure attribute over http, got %q", set)
		}
	}

	// Proxy HTTPS via X-Forwarded-Proto: should set Secure
	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", bytes.NewReader(body))
		req.Header.Set("X-Forwarded-Proto", "https")
		a.HandleLogin(rec, req)
		set := rec.Header().Get("Set-Cookie")
		if !strings.Contains(strings.ToLower(set), "secure") {
			t.Fatalf("expected Secure attribute when forwarded proto is https, got %q", set)
		}
	}
}
