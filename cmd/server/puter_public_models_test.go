package main

import (
	"slices"
	"testing"
)

func TestNormalizePuterModelID(t *testing.T) {
	tests := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "anthropic:anthropic/claude-opus-4-6", want: "claude-opus-4-6", ok: true},
		{raw: "deepseek:deepseek/deepseek-chat", want: "deepseek-chat", ok: true},
		{raw: "mistralai:mistralai/mistral-large-2512", want: "mistral-large-2512", ok: true},
		{raw: "x-ai:x-ai/grok-4", want: "grok-4", ok: true},
		{raw: "openai:openai/gpt-5.4", want: "", ok: false},
		{raw: "google:google/gemini-3.1-pro-preview", want: "", ok: false},
		{raw: "openrouter:openai/gpt-5.4", want: "", ok: false},
		{raw: "togetherai:qwen/qwen3.5-397b-a17b", want: "", ok: false},
	}

	for _, tt := range tests {
		got, ok := normalizePuterModelID(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("normalizePuterModelID(%q)=(%q,%v) want (%q,%v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestNormalizePuterPublicModels_DedupsAndFilters(t *testing.T) {
	got := normalizePuterPublicModels([]string{
		"openai:openai/gpt-5.4",
		"openai:openai/gpt-5.4",
		"anthropic:anthropic/claude-opus-4-6",
		"deepseek:deepseek/deepseek-chat",
		"mistralai:mistralai/mistral-large-2512",
		"openrouter:openai/gpt-5.4",
		"togetherai:qwen/qwen3.5-397b-a17b",
		"x-ai:x-ai/grok-4",
	})

	ids := make([]string, 0, len(got))
	for _, item := range got {
		ids = append(ids, item.ID)
	}

	want := []string{"claude-opus-4-6", "deepseek-chat", "grok-4", "mistral-large-2512"}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids=%v want %v", ids, want)
	}
}
