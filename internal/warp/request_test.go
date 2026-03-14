package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestBuildRequestBytes_UsesCodeFreeMaxStylePrompt(t *testing.T) {
	req := upstream.UpstreamRequest{
		Prompt:  "ignored because messages are present",
		Model:   "claude-4-5-sonnet",
		Workdir: "/repo",
		Messages: []prompt.Message{
			{
				Role: "user",
				Content: prompt.MessageContent{
					Text: "check the project layout",
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "text", Text: "I will inspect the repository."},
						{Type: "tool_use", ID: "call_1", Name: "Glob", Input: map[string]interface{}{"pattern": "**/*"}},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "./README.md\n./main.go"},
					},
				},
			},
		},
	}

	promptText, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("expected protobuf payload")
	}
	if !strings.Contains(promptText, "<|system_prompt|>") {
		t.Fatalf("prompt missing system prompt header: %q", promptText)
	}
	if strings.Contains(promptText, "Current working directory: /repo") {
		t.Fatalf("prompt should not include workdir: %q", promptText)
	}
	if strings.Contains(promptText, "<tool_call name=\"Glob\" id=\"call_1\">") {
		t.Fatalf("prompt should not include assistant tool call transcript: %q", promptText)
	}
	if !strings.Contains(promptText, "<|tool_result:call_1|>") {
		t.Fatalf("prompt missing tool result transcript: %q", promptText)
	}
	if !strings.Contains(promptText, "When executing commands, show the command and explain the output.") {
		t.Fatalf("prompt missing CodeFreeMax output guidance: %q", promptText)
	}
}

func TestEstimateInputTokens_CodeFreeMaxProfile(t *testing.T) {
	estimate, err := EstimateInputTokens("say hi", "gpt-4o", nil, nil, false)
	if err != nil {
		t.Fatalf("EstimateInputTokens error: %v", err)
	}
	if estimate.Profile != "warp-codefreemax" {
		t.Fatalf("profile=%q want warp-codefreemax", estimate.Profile)
	}
	if estimate.Total <= 0 {
		t.Fatalf("expected positive total tokens, got %d", estimate.Total)
	}
}
