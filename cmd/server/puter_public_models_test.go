package main

import (
	"slices"
	"testing"
)

func TestNormalizePuterPublicModelDetailsKeepsOnlyCurrentPolicy(t *testing.T) {
	got := normalizePuterPublicModelDetails([]puterPublicModelDetails{
		{ID: "claude-opus-5", Name: "Claude Opus 5"},
		{ID: "CLAUDE-OPUS-5", Name: "duplicate"},
		{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash"},
		{ID: "deepseek-v4-flash", Name: ""},
		{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
		{ID: "openrouter:openai/gpt-5.6", Name: "OpenRouter GPT"},
	})

	ids := make([]string, 0, len(got))
	for _, item := range got {
		ids = append(ids, item.ID)
	}
	want := []string{"claude-opus-5", "deepseek-v4-flash", "gemini-3.5-flash"}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids=%v want %v", ids, want)
	}
	if got[1].Name != "deepseek-v4-flash" {
		t.Fatalf("empty display name fallback=%q", got[1].Name)
	}
}
