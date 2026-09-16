package main

import (
	"slices"
	"testing"
)

// TestNormalizePuterPublicModelDetailsPassesTheCatalogThrough proves the reader
// publishes exactly what the upstream catalog returned.
//
// It used to narrow the list to a compiled-in generation, which meant a model
// the account could actually run stayed invisible until this repository was
// edited. Deduplication and the empty-name fallback are the only transforms left.
func TestNormalizePuterPublicModelDetailsPassesTheCatalogThrough(t *testing.T) {
	got := normalizePuterPublicModelDetails([]puterPublicModelDetails{
		{ID: "claude-opus-5", Name: "Claude Opus 5"},
		{ID: "CLAUDE-OPUS-5", Name: "duplicate"},
		{ID: "gemini-3.5-flash", Name: "Gemini 3.5 Flash"},
		{ID: "deepseek-v4-flash", Name: ""},
		// A model the compiled-in policy list never carried must survive: the
		// upstream catalog is the availability source.
		{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
		{ID: "openrouter:openai/gpt-5.6", Name: "OpenRouter GPT"},
	})

	ids := make([]string, 0, len(got))
	for _, item := range got {
		ids = append(ids, item.ID)
	}
	want := []string{
		"claude-opus-4-6",
		"claude-opus-5",
		"deepseek-v4-flash",
		"gemini-3.5-flash",
		"openrouter:openai/gpt-5.6",
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids=%v want %v", ids, want)
	}
	for _, item := range got {
		if item.ID == "deepseek-v4-flash" && item.Name != "deepseek-v4-flash" {
			t.Fatalf("empty display name fallback=%q", item.Name)
		}
		if item.ID == "" {
			t.Fatal("an empty identifier was published")
		}
	}
}
