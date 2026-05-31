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

func TestChatPayload_UsesCurrentAppChatModelFields(t *testing.T) {
	c := New(nil)
	spec := ModelSpec{
		ID:            "grok-imagine-image-lite",
		UpstreamModel: "grok-imagine-image-lite",
		ModelMode:     "MODEL_MODE_FAST",
		Tier:          grokTierBasic,
		IsImage:       true,
	}

	payload := c.chatPayload(spec, "draw an apple", true, 1)

	if got, _ := payload["modeId"].(string); got != "fast" {
		t.Fatalf("modeId=%q want fast", got)
	}
	if got, _ := payload["modelTier"].(string); got != "basic" {
		t.Fatalf("modelTier=%q want basic", got)
	}
	if got, _ := payload["modelMode"].(string); got != "MODEL_MODE_FAST" {
		t.Fatalf("modelMode=%q want legacy compatibility value", got)
	}
	if _, ok := payload["collectionIds"].([]string); !ok {
		t.Fatalf("collectionIds missing")
	}
	if _, ok := payload["connectors"].([]string); !ok {
		t.Fatalf("connectors missing")
	}
	toolOverrides, ok := payload["toolOverrides"].(map[string]interface{})
	if !ok {
		t.Fatalf("toolOverrides missing")
	}
	if got, _ := toolOverrides["webSearch"].(bool); got {
		t.Fatalf("webSearch=%v want false for image generation", got)
	}
	if got, _ := toolOverrides["xSearch"].(bool); got {
		t.Fatalf("xSearch=%v want false for image generation", got)
	}
	textPayload := c.chatPayload(ModelSpec{ID: "grok-4.20-fast", UpstreamModel: "grok-4.20-fast"}, "hello", true, 0)
	textOverrides := textPayload["toolOverrides"].(map[string]interface{})
	if got, _ := textOverrides["webSearch"].(bool); !got {
		t.Fatalf("webSearch=%v want true for text chat", got)
	}
	if got, _ := textOverrides["xSearch"].(bool); !got {
		t.Fatalf("xSearch=%v want true for text chat", got)
	}
}

func TestAppChatModeID_UsesCustomModeID(t *testing.T) {
	spec := ModelSpec{ID: "grok-custom-app-chat", UpstreamModel: "grok-custom-app-chat", ModelMode: "grok-custom-mode", Tier: grokTierSuper}

	if got := appChatModeID(spec); got != "grok-custom-mode" {
		t.Fatalf("appChatModeID()=%q want custom mode", got)
	}
	if got := appChatModelTier(spec); got != "super" {
		t.Fatalf("appChatModelTier()=%q want super", got)
	}
}
