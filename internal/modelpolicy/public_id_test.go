package modelpolicy

import "testing"

func TestProviderScopedPublicID(t *testing.T) {
	for _, tc := range []struct {
		provider string
		public   string
		want     string
	}{
		{"build", "grok-4.7", "build/grok-4.7"},
		// An already-qualified name is left alone, and an unknown plane is not
		// given a qualifier it never had.
		{"", "grok-4.6", "grok-4.6"},
		{"warp", "grok-4.6", "grok-4.6"},
	} {
		if got := ProviderScopedPublicID(tc.provider, tc.public); got != tc.want {
			t.Fatalf("ProviderScopedPublicID(%q, %q) = %q want %q", tc.provider, tc.public, got, tc.want)
		}
	}
}
