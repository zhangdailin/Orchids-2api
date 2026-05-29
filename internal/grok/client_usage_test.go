package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

func TestGetUsage_DefaultModelFallbackToGrok420Fast(t *testing.T) {
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
	if !info.HasLimit || !info.HasRemaining || info.Unit != "requests" {
		t.Fatalf("unexpected usage metadata: %#v", info)
	}
	if len(requestedModels) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requestedModels))
	}
	if requestedModels[1] != "fast" {
		t.Fatalf("expected fallback rate-limit model fast, got %q", requestedModels[1])
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
	_, err := c.GetUsage(context.Background(), "token-abc", "grok-4.20-0309-reasoning")
	if err == nil {
		t.Fatalf("expected error for explicit invalid model")
	}
	if len(requestedModels) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requestedModels))
	}
	if requestedModels[0] != "expert" {
		t.Fatalf("expected explicit reasoning model to use expert rate-limit mode, got=%v", requestedModels)
	}
}

func TestRateLimitModelName_UsesModeAcceptedByUpstream(t *testing.T) {
	tests := []struct {
		name string
		spec ModelSpec
		want string
	}{
		{
			name: "fast mode",
			spec: ModelSpec{UpstreamModel: "grok-4.20-0309-non-reasoning", ModelMode: "MODEL_MODE_FAST"},
			want: "fast",
		},
		{
			name: "auto mode",
			spec: ModelSpec{UpstreamModel: "grok-4.20-0309", ModelMode: "MODEL_MODE_AUTO"},
			want: "auto",
		},
		{
			name: "grok 4.3 custom mode",
			spec: ModelSpec{UpstreamModel: "grok-4.3-beta", ModelMode: "grok-420-computer-use-sa"},
			want: "grok-420-computer-use-sa",
		},
		{
			name: "fallback upstream",
			spec: ModelSpec{UpstreamModel: "custom-model"},
			want: "custom-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rateLimitModelName(tt.spec); got != tt.want {
				t.Fatalf("rateLimitModelName()=%q want %q", got, tt.want)
			}
		})
	}
}
