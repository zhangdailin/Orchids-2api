package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
)

func TestHandleChatCompletionsChecksAutoMappedImageModelPolicy(t *testing.T) {
	h := &Handler{}
	wrapper := middleware.APIKeyAuthWithRequest(
		func(*http.Request) bool { return true },
		func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, nil
		},
		h.HandleChatCompletions,
	)
	body := "{\"model\":\"grok-4.6\",\"messages\":[{\"role\":\"user\",\"content\":\"draw a cat\"}],\"image_config\":{\"n\":1}}"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	wrapper(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleImagesGenerationsChecksAPIKeyModelPolicy(t *testing.T) {
	h := &Handler{}
	wrapper := middleware.APIKeyAuthWithRequest(
		func(*http.Request) bool { return true },
		func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, nil
		},
		h.HandleImagesGenerations,
	)
	body := "{\"model\":\"grok-imagine-image\",\"prompt\":\"cat\"}"
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	wrapper(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
