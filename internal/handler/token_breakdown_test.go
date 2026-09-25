package handler

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
)

func TestEstimateInputTokenBreakdown_SplitsSystemContext(t *testing.T) {
	t.Parallel()

	prompt := "<env>\ndate: 2026-02-12\n</env>\n<rules>\n- concise\n</rules>\n<sys>\nproject context and constraints\n</sys>\n<user>\nhello\n</user>"
	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "Read",
				"parameters": map[string]interface{}{
					"type": "object",
				},
			},
		},
	}

	bd := estimateInputTokenBreakdown(prompt, tools)
	if bd.SystemContextTokens <= 0 {
		t.Fatalf("expected system_context tokens > 0")
	}
	if bd.BasePromptTokens <= 0 {
		t.Fatalf("expected base prompt tokens > 0")
	}
	if bd.ToolsTokens <= 0 {
		t.Fatalf("expected tools tokens > 0")
	}
	if bd.Total != bd.BasePromptTokens+bd.SystemContextTokens+bd.HistoryTokens+bd.ToolsTokens {
		t.Fatalf("unexpected total=%d", bd.Total)
	}
}

func TestHandleCountTokens_ReturnsBreakdown(t *testing.T) {
	t.Parallel()

	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:   false,
		DebugLogSSE:    false,
		RequestTimeout: 30,
	}, nil)

	reqBody := map[string]interface{}{
		"model": "claude-3-5-sonnet",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "What is dependency injection?"},
		},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name": "Read",
					"parameters": map[string]interface{}{
						"type": "object",
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(reqBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/v1/messages/count_tokens", bytes.NewReader(raw))

	h.HandleCountTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, ok := resp["input_tokens"].(float64); !ok || v <= 0 {
		t.Fatalf("expected positive input_tokens, got %#v", resp["input_tokens"])
	}
	if _, ok := resp["prompt_profile"].(string); !ok {
		t.Fatalf("expected prompt_profile string, got %#v", resp["prompt_profile"])
	}
	breakdown, ok := resp["breakdown"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected breakdown object, got %#v", resp["breakdown"])
	}
	required := []string{"base_prompt_tokens", "system_context_tokens", "history_tokens", "tools_tokens"}
	for _, key := range required {
		if _, ok := breakdown[key].(float64); !ok {
			t.Fatalf("expected breakdown key %q as number, got %#v", key, breakdown[key])
		}
	}
	if toolsTokens, _ := breakdown["tools_tokens"].(float64); toolsTokens <= 0 {
		t.Fatalf("expected positive tools_tokens, got %#v", breakdown["tools_tokens"])
	}
}
