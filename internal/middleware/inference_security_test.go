package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBearerOnlyAndRateLimit(t *testing.T) {
	calls := 0
	h := BearerAPIKeyAuth(func(_ context.Context, token string) (*APIKeyPrincipal, error) {
		calls++
		if token == "broken" {
			return nil, errors.New("private-upstream-secret")
		}
		return &APIKeyPrincipal{ID: 1}, nil
	}, nil, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, header := range []string{"", "secret", "Basic secret", "Bearer", "Bearer a b"} {
		r := httptest.NewRequest("GET", "/v1/models?api_key=secret", nil)
		r.Header.Set("Authorization", header)
		r.Header.Set("x-api-key", "secret")
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code != 401 {
			t.Fatalf("non-Bearer accepted: %q", header)
		}
	}
	if calls != 0 {
		t.Fatal("invalid header reached credential store")
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer broken")
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "private-upstream-secret") {
		t.Fatal("auth error leaked")
	}
	h = BearerAPIKeyAuth(func(context.Context, string) (*APIKeyPrincipal, error) { return &APIKeyPrincipal{ID: 1}, nil }, NewRateLimiter(1, time.Hour), func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	r.Header.Set("Authorization", "Bearer valid")
	for _, status := range []int{204, 429} {
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code != status {
			t.Fatalf("got %d want %d", rec.Code, status)
		}
	}
}

func TestInferenceErrorBodiesAndStreaming(t *testing.T) {
	h := InferenceErrors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(502)
		_, _ = w.Write([]byte("private-"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("upstream-secret"))
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/v1/responses", nil))
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "private") || rec.Header().Get("Content-Length") != "" || rec.Header().Get("Retry-After") != "7" {
		t.Fatal("error response was not sanitized")
	}
	h = InferenceErrors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: hello\n\n"))
		w.(http.Flusher).Flush()
		MarkStreamFailure(w)
	})
	rec = httptest.NewRecorder()
	trace := NewTracedResponseWriter(rec)
	h(trace, httptest.NewRequest("POST", "/v1/responses", nil))
	if rec.Body.String() != "data: hello\n\n" || !rec.Flushed || !trace.StreamFailed() {
		t.Fatal("stream semantics changed")
	}
}
