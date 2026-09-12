package util

import "testing"

// TestFingerprint_StableAndNonReversible pins the two properties the account
// table relies on: the same secret always maps to the same short id, different
// secrets map to different ids, and the digest never contains the secret.
func TestFingerprint_StableAndNonReversible(t *testing.T) {
	token := "warp-session-token-abcdef"
	first := Fingerprint(token)
	second := Fingerprint("  " + token + "  ")
	if first == "" || len(first) != 12 {
		t.Fatalf("Fingerprint() = %q, want 12 hex characters", first)
	}
	if first != second {
		t.Fatalf("fingerprint is not stable across whitespace: %q vs %q", first, second)
	}
	if other := Fingerprint("warp-session-token-abcdeg"); other == first {
		t.Fatal("different secrets must not share a fingerprint")
	}
	if Fingerprint("") != "" {
		t.Fatal("an empty secret has no fingerprint")
	}
}
