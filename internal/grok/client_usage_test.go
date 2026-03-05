package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

func TestGetUsage_DefaultModelFallbackToGrok3(t *testing.T) {
	t.Parallel()

	var requestedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRateLimitsPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		model, _ := payload["modelName"].(string)
		requestedModels = append(requestedModels, model)

		if len(requestedModels) == 1 {
			http.Error(w, `{"error":{"message":"Model is not found"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"maxQueries":120,"remainingQueries":33}`))
	}))
	defer srv.Close()

	c := New(&config.Config{GrokAPIBaseURL: srv.URL})
	info, err := c.GetUsage(context.Background(), "sso=token-abc; Path=/; HttpOnly", "")
	if err != nil {
		t.Fatalf("GetUsage() error: %v", err)
	}
	if info == nil {
		t.Fatalf("GetUsage() returned nil info")
	}
	if info.Limit != 120 || info.Remaining != 33 {
		t.Fatalf("unexpected info: limit=%d remaining=%d", info.Limit, info.Remaining)
	}
	if len(requestedModels) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requestedModels))
	}
	if requestedModels[1] != "grok-3" {
		t.Fatalf("expected fallback model grok-3, got %q", requestedModels[1])
	}
}

func TestGetUsage_ExplicitModelDoesNotFallback(t *testing.T) {
	t.Parallel()

	var requestedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRateLimitsPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		model, _ := payload["modelName"].(string)
		requestedModels = append(requestedModels, model)
		http.Error(w, `{"error":{"message":"Model is not found"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(&config.Config{GrokAPIBaseURL: srv.URL})
	_, err := c.GetUsage(context.Background(), "token-abc", "grok-4-1-thinking-1129")
	if err == nil {
		t.Fatalf("expected error for explicit invalid model")
	}
	if len(requestedModels) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requestedModels))
	}
	if requestedModels[0] != "grok-4.1-thinking-1129" {
		t.Fatalf("expected request model to stay explicit, got=%v", requestedModels)
	}
}
