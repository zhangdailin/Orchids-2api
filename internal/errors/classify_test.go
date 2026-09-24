package errors

import (
	"testing"
)

func TestClassifyUpstreamError(t *testing.T) {
	tests := []struct {
		name         string
		errStr       string
		wantCategory string
		wantRetry    bool
		wantSwitch   bool
	}{
		{
			name:         "model not found is client error",
			errStr:       "puter API error: message=Model not found, please try another model",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "no implementation available is client error",
			errStr:       "puter API error: code=no_implementation_available, status=502, message=No implementation available for interface `puter-chat-completion`.",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "insufficient funds is quota exhausted",
			errStr:       "puter API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.",
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp quota limit is quota exhausted",
			errStr:       "warp stream finished with quota_limit: no remaining quota",
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp no ai credits is quota exhausted",
			errStr:       `warp stream request failed: HTTP 429 [OUT_OF_CREDITS]: {"error":"No AI credits remaining"}`,
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp context window is client error",
			errStr:       "warp stream finished with context_window_exceeded: input is too long",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "warp invalid api key switches account",
			errStr:       "warp stream finished with invalid_api_key: provider=openai model=gpt-test",
			wantCategory: "auth",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp llm unavailable is server error",
			errStr:       "warp stream finished with llm_unavailable: model unavailable",
			wantCategory: "model_unavailable",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp 400 model not allowed switches account",
			errStr:       `warp stream request failed: HTTP 400: {"error":"Invalid request: the requested base model (claude-4-5-opus) is not allowed for your account"}`,
			wantCategory: "model_unavailable",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "qoder agent limit reset is model rate limit",
			errStr:       `qoder agent limit reached; resets at 2026-09-27T19:47:13Z`,
			wantCategory: "rate_limit",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "qoder busy is a terminal request-level rate limit",
			errStr:       `qoder gateway is busy: {"code":"10605","serviceAvailable":false,"retryAfterSeconds":29}`,
			wantCategory: "rate_limit",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "qoder duplicate request is not retried",
			errStr:       "qoder duplicate request",
			wantCategory: "client",
		},
		{
			name:         "warp 400 no model available switches account",
			errStr:       `warp stream request failed: HTTP 400: {"error":"Invalid request: the requested base model (gemini-3-1-pro) has no model available"}`,
			wantCategory: "model_unavailable",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "warp max token limit is client error",
			errStr:       "warp stream finished with max_token_limit: maximum output tokens reached",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyUpstreamError(tt.errStr)
			if got.Category != tt.wantCategory || got.Retryable != tt.wantRetry || got.SwitchAccount != tt.wantSwitch {
				t.Fatalf("ClassifyUpstreamError(%q) = %#v, want category=%q retry=%v switch=%v", tt.errStr, got, tt.wantCategory, tt.wantRetry, tt.wantSwitch)
			}
		})
	}
}
