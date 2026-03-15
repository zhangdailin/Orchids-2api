package handler

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestEstimateInputTokenBreakdown_SplitsSystemContext(t *testing.T) {
	t.Parallel()

	prompt := "<env>\ndate: 2026-02-12\n</env>\n<rules>\n- concise\n</rules>\n<sys>\nproject context and constraints\n</sys>\n<user>\nhello\n</user>"
	history := []map[string]string{
		{"role": "user", "content": "previous context message"},
	}
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

	bd := estimateInputTokenBreakdown(prompt, history, tools)
	if bd.SystemContextTokens <= 0 {
		t.Fatalf("expected system_context tokens > 0")
	}
	if bd.BasePromptTokens <= 0 {
		t.Fatalf("expected base prompt tokens > 0")
	}
	if bd.HistoryTokens <= 0 {
		t.Fatalf("expected history tokens > 0")
	}
	if bd.ToolsTokens <= 0 {
		t.Fatalf("expected tools tokens > 0")
	}
	if bd.Total != bd.BasePromptTokens+bd.SystemContextTokens+bd.HistoryTokens+bd.ToolsTokens {
		t.Fatalf("unexpected total=%d", bd.Total)
	}
}

func TestEstimateOrchidsInputTokenBreakdown_ExcludesTools(t *testing.T) {
	t.Parallel()

	prompt := "<sys>\nsystem\n</sys>\n<user>\nhello\n</user>"
	history := []map[string]string{
		{"role": "assistant", "content": "previous reply"},
	}

	bd := estimateOrchidsInputTokenBreakdown(prompt, history)
	if bd.BasePromptTokens <= 0 {
		t.Fatalf("expected base prompt tokens > 0")
	}
	if bd.HistoryTokens <= 0 {
		t.Fatalf("expected history tokens > 0")
	}
	if bd.ToolsTokens != 0 {
		t.Fatalf("expected orchids tools tokens = 0, got %d", bd.ToolsTokens)
	}
}

func TestHandleCountTokens_ReturnsBreakdown(t *testing.T) {
	t.Parallel()

	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:            false,
		DebugLogSSE:             false,
		ContextMaxTokens:        12000,
		RequestTimeout:          30,
		ContextKeepTurns:        2,
		ContextSummaryMaxTokens: 256,
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
	if profile, ok := resp["prompt_profile"].(string); !ok {
		t.Fatalf("expected prompt_profile string, got %#v", resp["prompt_profile"])
	} else if profile != "orchids-protocol" {
		t.Fatalf("expected orchids profile, got %#v", resp["prompt_profile"])
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
	if toolsTokens, _ := breakdown["tools_tokens"].(float64); toolsTokens != 0 {
		t.Fatalf("expected orchids tools_tokens=0, got %#v", breakdown["tools_tokens"])
	}
}

func TestHandleCountTokens_WarpUsesWarpEstimator(t *testing.T) {
	t.Parallel()

	h := NewWithLoadBalancer(&config.Config{
		DebugEnabled:            false,
		DebugLogSSE:             false,
		ContextMaxTokens:        12000,
		RequestTimeout:          30,
		ContextKeepTurns:        2,
		ContextSummaryMaxTokens: 256,
	}, nil)

	reqBody := map[string]interface{}{
		"model": "auto-efficient",
		"messages": []map[string]interface{}{
			{"role": "user", "content": "当前目录"},
		},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "Bash",
					"description": "Run shell command",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"command": map[string]interface{}{"type": "string"},
						},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(reqBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages/count_tokens", bytes.NewReader(raw))

	h.HandleCountTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if profile, _ := resp["prompt_profile"].(string); !strings.HasPrefix(profile, "warp") {
		t.Fatalf("expected warp profile, got %#v", resp["prompt_profile"])
	}
	breakdown, ok := resp["breakdown"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected breakdown object, got %#v", resp["breakdown"])
	}
	if v, ok := breakdown["tools_tokens"].(float64); !ok || v != 0 {
		t.Fatalf("expected warp tools_tokens = 0 in CodeFreeMax mode, got %#v", breakdown["tools_tokens"])
	}
	if v, ok := breakdown["system_context_tokens"].(float64); !ok || v != 0 {
		t.Fatalf("expected warp system_context_tokens = 0, got %#v", breakdown["system_context_tokens"])
	}
}
