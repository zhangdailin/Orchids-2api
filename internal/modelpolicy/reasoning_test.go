package modelpolicy

import "testing"

func TestSupportedReasoningEfforts(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"grok-4.6", []string{"low", "medium", "high", "xhigh"}},
		{"grok-4.5", []string{"low", "medium", "high"}},
		{"grok-3-mini-fast", []string{"low", "medium", "high"}},
		{"grok-build-0.1", []string{"none"}},
		{"grok-unknown-model", []string{"none"}},
		// Provider prefixes are stripped before lookup.
		{"console/grok-4.6", []string{"low", "medium", "high", "xhigh"}},
		// The Console reasoning variant is fixed: it reasons but takes no effort.
		{"console/grok-4.20-0309-reasoning", nil},
		// The same slug on another plane keeps its configurable levels.
		{"grok-4.20-0309-reasoning", []string{"low", "medium", "high"}},
	} {
		got := SupportedReasoningEfforts(tc.model)
		if len(got) != len(tc.want) {
			t.Fatalf("%s levels=%v want %v", tc.model, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s levels=%v want %v", tc.model, got, tc.want)
			}
		}
	}
}

func TestSupportsReasoningEffortAndComposer(t *testing.T) {
	if !SupportsReasoningEffort("grok-4.6", "XHIGH") {
		t.Fatal("grok-4.6 should accept xhigh")
	}
	if SupportsReasoningEffort("grok-4.5", "xhigh") {
		t.Fatal("grok-4.5 has no xhigh contract")
	}
	for _, model := range []string{"grok-composer-2.5-fast", "console/grok-composer-2.5-fast"} {
		if !IsGrokComposerModel(model) {
			t.Fatalf("%s should be recognised as Composer", model)
		}
	}
	if IsGrokComposerModel("grok-4.6") {
		t.Fatal("grok-4.6 is not Composer")
	}
}
