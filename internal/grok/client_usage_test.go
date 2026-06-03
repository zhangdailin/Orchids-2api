package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

func TestGetUsage_DefaultModelDoesNotFallback(t *testing.T) {
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

		http.Error(w, `{"error":{"message":"Model is not found"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(&config.Config{GrokAPIBaseURL: srv.URL})
	if _, err := c.GetUsage(context.Background(), "sso=token-abc; Path=/; HttpOnly", ""); err == nil {
		t.Fatalf("expected error for default model rejection")
	}
	if len(requestedModels) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requestedModels))
	}
	if requestedModels[0] != "auto" {
		t.Fatalf("expected default model to use auto rate-limit mode, got %q", requestedModels[0])
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
	if _, ok := payload["disabledConnectorIds"].([]string); !ok {
		t.Fatalf("disabledConnectorIds missing")
	}
	toolOverrides, ok := payload["toolOverrides"].(map[string]interface{})
	if !ok {
		t.Fatalf("toolOverrides missing for image generation")
	}
	if got, _ := toolOverrides["imageGen"].(bool); !got {
		t.Fatalf("imageGen=%v want true for image generation", got)
	}
	textPayload := c.chatPayload(ModelSpec{ID: "grok-4.20-fast", UpstreamModel: "grok-4.20-fast"}, "hello", true, 0)
	if _, ok := textPayload["toolOverrides"]; ok {
		t.Fatalf("toolOverrides should be omitted for default text chat: %#v", textPayload["toolOverrides"])
	}
	if got, _ := textPayload["disableSearch"].(bool); got {
		t.Fatalf("disableSearch=%v want false for text chat", got)
	}
	if got, _ := textPayload["linkQuery"].(bool); got {
		t.Fatalf("linkQuery=%v want false", got)
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

func TestGetVoiceToken_UsesPersonalityWhenInstructionEmpty(t *testing.T) {
	t.Parallel()

	var session map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultLivekitPath {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if got, _ := payload["livekitUrl"].(string); got != "wss://livekit.grok.com" {
			t.Fatalf("livekitUrl=%q", got)
		}
		rawSession, _ := payload["sessionPayload"].(string)
		if err := json.Unmarshal([]byte(rawSession), &session); err != nil {
			t.Fatalf("decode sessionPayload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"lk-token","livekitUrl":"wss://custom.livekit"}`))
	}))
	defer srv.Close()

	c := New(&config.Config{GrokAPIBaseURL: srv.URL})
	data, err := c.getVoiceToken(context.Background(), "token-abc", "eve", "therapist", 1.2, "")
	if err != nil {
		t.Fatalf("getVoiceToken() error: %v", err)
	}
	if got, _ := data["token"].(string); got != "lk-token" {
		t.Fatalf("token=%q", got)
	}
	if got, _ := session["voice"].(string); got != "eve" {
		t.Fatalf("voice=%q", got)
	}
	if got, _ := session["personality"].(string); got != "therapist" {
		t.Fatalf("personality=%q", got)
	}
	if _, ok := session["instructions"]; ok {
		t.Fatalf("instructions should be omitted: %#v", session)
	}
	if _, ok := session["is_raw_instructions"]; ok {
		t.Fatalf("is_raw_instructions should be omitted: %#v", session)
	}
}

func TestGetVoiceToken_UsesRawInstructionsWhenProvided(t *testing.T) {
	t.Parallel()

	var session map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		rawSession, _ := payload["sessionPayload"].(string)
		if err := json.Unmarshal([]byte(rawSession), &session); err != nil {
			t.Fatalf("decode sessionPayload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"lk-token"}`))
	}))
	defer srv.Close()

	c := New(&config.Config{GrokAPIBaseURL: srv.URL})
	_, err := c.getVoiceToken(context.Background(), "token-abc", "ara", "assistant", 1, "  speak Chinese  ")
	if err != nil {
		t.Fatalf("getVoiceToken() error: %v", err)
	}
	if got, _ := session["personality"].(string); got != "assistant" {
		t.Fatalf("personality=%q", got)
	}
	if _, ok := session["instructions"]; ok {
		t.Fatalf("instructions should be omitted: %#v", session)
	}
	if _, ok := session["is_raw_instructions"]; ok {
		t.Fatalf("is_raw_instructions should be omitted: %#v", session)
	}
}
