package modelpolicy

import (
	"slices"
	"testing"
)

func TestLatestPuterModels(t *testing.T) {
	models := LatestPuterModelIDs()
	if len(models) == 0 || models[0] != DefaultPuterModelID {
		t.Fatalf("LatestPuterModelIDs() starts with %q, want %q", models[0], DefaultPuterModelID)
	}
	seen := map[string]bool{}
	for _, id := range models {
		if seen[id] {
			t.Fatalf("duplicate Puter model %q", id)
		}
		seen[id] = true
		if !IsLatestPuterModelID(id) {
			t.Fatalf("IsLatestPuterModelID(%q) = false", id)
		}
	}
	if IsLatestPuterModelID("claude-opus-4-5") {
		t.Fatal("old Puter model unexpectedly allowed")
	}
	want := []string{
		"claude-opus-5", "claude-sonnet-5", "claude-fable-5",
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gemini-3.5-flash", "grok-4.5", "deepseek-v4-pro",
		"deepseek-v4-flash", "mistral-small-2603",
	}
	if !slices.Equal(models, want) {
		t.Fatalf("LatestPuterModelIDs()=%v want=%v", models, want)
	}
	for _, removed := range []string{
		"claude-opus-5-fast", "gpt-5.6-sol-pro", "gpt-5.6-terra-pro",
		"gpt-5.6-luna-pro", "gemini-3.6-flash", "gemini-3.5-flash-lite",
	} {
		if IsLatestPuterModelID(removed) {
			t.Fatalf("catalog-missing Puter model %q unexpectedly allowed", removed)
		}
	}
}
