package modelpolicy

import "testing"

func TestPuterServiceForModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":      "claude",
		"gpt-5.6-sol":        "openai",
		"gemini-3.5-flash":   "google",
		"grok-4.5":           "x-ai",
		"deepseek-v4-pro":    "deepseek",
		"mistral-small-2603": "mistral",
	}
	for model, want := range cases {
		got, ok := PuterServiceForModel(model)
		if !ok || got != want {
			t.Fatalf("PuterServiceForModel(%q) = (%q, %v), want (%q, true)", model, got, ok, want)
		}
	}
	// An identifier of this gateway's own making has no upstream service. The
	// catalog decides what exists; routing refuses what it cannot map.
	for _, unknown := range []string{"", "  ", "llama-4", "custom/model"} {
		if _, ok := PuterServiceForModel(unknown); ok {
			t.Fatalf("PuterServiceForModel(%q) unexpectedly resolved", unknown)
		}
	}
}
