package api

import "testing"

func TestIsLikelyJWT(t *testing.T) {
	if isLikelyJWT("") {
		t.Fatalf("empty should be false")
	}
	if isLikelyJWT("abc") {
		t.Fatalf("no dots should be false")
	}
	if !isLikelyJWT("aaaaaaaaaa.bbbbbbbbbb.cccccccccc") {
		t.Fatalf("expected true")
	}
}
