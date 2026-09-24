package modelpolicy

import "testing"

func TestProviderScopedPublicID(t *testing.T) {
	for _, tc := range []struct {
		provider string
		public   string
		want     string
	}{
		{"console", "grok-4.20-0309-reasoning", "console/grok-4.20-0309-reasoning"},
		{"Console", "GROK-4.6", "console/grok-4.6"},
		{"web", "grok-chat-auto", "web/grok-chat-auto"},
		{"build", "grok-4.7", "build/grok-4.7"},
		// An already-qualified name is left alone, and an unknown plane is not
		// given a qualifier it never had.
		{"console", "console/grok-4.5", "console/grok-4.5"},
		{"", "grok-4.6", "grok-4.6"},
		{"warp", "grok-4.6", "grok-4.6"},
	} {
		if got := ProviderScopedPublicID(tc.provider, tc.public); got != tc.want {
			t.Fatalf("ProviderScopedPublicID(%q, %q) = %q want %q", tc.provider, tc.public, got, tc.want)
		}
	}
}

// TestProviderScopedPublicIDKeepsFixedEffortModelsSilent is the invariant the
// public model list depends on: the effort aliases it derives from a row have to
// be the ones the resolver accepts. A row on the Console plane must be asked
// about with its qualifier, or a fixed-effort model advertises levels the
// resolver then rejects.
func TestProviderScopedPublicIDKeepsFixedEffortModelsSilent(t *testing.T) {
	fixed := "grok-4.20-0309-reasoning"
	if levels := SupportedReasoningEfforts(ProviderScopedPublicID("console", fixed)); len(levels) != 0 {
		t.Fatalf("console %s advertises %v through its plane", fixed, levels)
	}
	if levels := SupportedReasoningEfforts(fixed); len(levels) < 2 {
		t.Fatalf("the bare name keeps its own contract: %v", levels)
	}
	// A Console model that does expose levels still advertises them.
	if levels := SupportedReasoningEfforts(ProviderScopedPublicID("console", "grok-4.3")); len(levels) < 2 {
		t.Fatalf("console grok-4.3 levels=%v", levels)
	}
}
