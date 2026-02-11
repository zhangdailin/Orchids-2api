package handler

import "testing"

func TestClassifyUpstreamErrorCreditsExhausted(t *testing.T) {
	t.Parallel()

	errClass := classifyUpstreamError("orchids upstream error: no remaining quota: You have run out of credits.")
	if errClass.category != "rate_limit" {
		t.Fatalf("expected rate_limit category, got %q", errClass.category)
	}
	if !errClass.retryable {
		t.Fatal("expected credits exhausted to be retryable")
	}
	if !errClass.switchAccount {
		t.Fatal("expected credits exhausted to trigger account switch")
	}
}
