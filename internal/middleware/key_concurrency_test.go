package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"orchids-api/internal/loadbalancer"
)

func TestAPIKeyConcurrencyRejectsSecondActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	next := APIKeyConcurrencyWithTracker(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	}, nil)
	wrapped := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*APIKeyPrincipal, error) {
		return &APIKeyPrincipal{ID: 7, MaxConcurrent: 1}, nil
	}, next)

	first := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	first.Header.Set("Authorization", "Bearer one")
	firstResult := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { wrapped(firstResult, first); close(done) }()
	<-entered

	second := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	second.Header.Set("Authorization", "Bearer one")
	secondResult := httptest.NewRecorder()
	wrapped(secondResult, second)
	if secondResult.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", secondResult.Code)
	}
	close(release)
	<-done
}

func TestAPIKeyConcurrencyUsesSharedAtomicTracker(t *testing.T) {
	tracker := loadbalancer.NewMemoryConnTracker()
	entered := make(chan struct{})
	release := make(chan struct{})
	next := APIKeyConcurrencyWithTracker(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}, tracker)
	wrapped := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*APIKeyPrincipal, error) {
		return &APIKeyPrincipal{ID: 9, MaxConcurrent: 1}, nil
	}, next)

	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer shared")
		wrapped(httptest.NewRecorder(), req)
		close(done)
	}()
	<-entered
	second := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	second.Header.Set("Authorization", "Bearer shared")
	recorder := httptest.NewRecorder()
	wrapped(recorder, second)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	close(release)
	<-done
	if got := tracker.GetCount(-9); got != 0 {
		t.Fatalf("shared key slot leaked: %d", got)
	}
}
