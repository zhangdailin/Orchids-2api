package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// count_tokens decides the token profile from the channel, and on the unified
// prefix the path names no channel at all. A path-only answer used to make every
// /v1 request fall through to the generic estimate while the completion itself
// ran on Warp, so a client that budgets its context against count_tokens planned
// against a number that did not belong to the provider serving it.
func TestHandleCountTokensUsesTheModelChannelWhenThePathHasNone(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "360", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)

	body := `{"model":"gpt-5-6-sol-medium","messages":[{"role":"user","content":"hello there, count my tokens"}]}`

	// The tracing middleware installs the hint box in production and the unified
	// dispatcher publishes the resolved model into it; count_tokens reads it back
	// so it never has to look the model up a second time.
	unified := httptest.NewRecorder()
	unifiedReq := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	unifiedCtx, _ := middleware.RequestModelHint(unifiedReq.Context())
	unifiedCtx = middleware.WithRequestModel(unifiedCtx, "gpt-5-6-sol-medium")
	h.HandleCountTokens(unified, unifiedReq.WithContext(unifiedCtx))
	if unified.Code != http.StatusOK {
		t.Fatalf("unified status = %d body=%s", unified.Code, unified.Body.String())
	}

	channelScoped := httptest.NewRecorder()
	h.HandleCountTokens(channelScoped, httptest.NewRequest(http.MethodPost, "/warp/v1/messages/count_tokens", strings.NewReader(body)))
	if channelScoped.Code != http.StatusOK {
		t.Fatalf("channel status = %d body=%s", channelScoped.Code, channelScoped.Body.String())
	}

	var unifiedBody, channelBody struct {
		InputTokens   int    `json:"input_tokens"`
		PromptProfile string `json:"prompt_profile"`
	}
	if err := json.Unmarshal(unified.Body.Bytes(), &unifiedBody); err != nil {
		t.Fatalf("decode unified: %v", err)
	}
	if err := json.Unmarshal(channelScoped.Body.Bytes(), &channelBody); err != nil {
		t.Fatalf("decode channel: %v", err)
	}

	if unifiedBody.PromptProfile != "warp-official-proto" {
		t.Fatalf("unified profile = %q, want the Warp profile its model resolves to", unifiedBody.PromptProfile)
	}
	if unifiedBody.PromptProfile != channelBody.PromptProfile {
		t.Fatalf("unified profile = %q, channel profile = %q; the unified prefix must resolve the channel from the model",
			unifiedBody.PromptProfile, channelBody.PromptProfile)
	}
	if unifiedBody.InputTokens != channelBody.InputTokens {
		t.Fatalf("unified tokens = %d, channel tokens = %d; both requests must estimate identically",
			unifiedBody.InputTokens, channelBody.InputTokens)
	}
}

// A model that resolves to no channel still has to answer: count_tokens is a
// budgeting call, and a 5xx there blocks the client before it ever sends the
// completion.
func TestHandleCountTokensAnswersForAnUnknownModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"who-knows","messages":[{"role":"user","content":"hi"}]}`))
	ctx, _ := middleware.RequestModelHint(req.Context())
	h.HandleCountTokens(rec, req.WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want a positive estimate", body.InputTokens)
	}
}
