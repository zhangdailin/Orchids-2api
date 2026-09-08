package handler

import (
	"testing"

	apperrors "orchids-api/internal/errors"
)

func TestClassifyUpstreamErrorCreditsExhausted(t *testing.T) {
	t.Parallel()

	errClass := apperrors.ClassifyUpstreamError("puter upstream error: no remaining quota: You have run out of credits.")
	if errClass.Category != "rate_limit" {
		t.Fatalf("expected rate_limit category, got %q", errClass.Category)
	}
	if !errClass.Retryable {
		t.Fatal("expected credits exhausted to be retryable")
	}
	if !errClass.SwitchAccount {
		t.Fatal("expected credits exhausted to trigger account switch")
	}
}

func TestShouldRetryCurrentAccountWhenNoAlternative_RateLimit(t *testing.T) {
	t.Parallel()

	if shouldRetryCurrentAccountWhenNoAlternative("rate_limit") {
		t.Fatal("expected rate_limit to stop retrying the same account when no alternative exists")
	}
}

func TestShouldRetryCurrentAccountWhenNoAlternative_ModelUnavailable(t *testing.T) {
	t.Parallel()

	if !shouldRetryCurrentAccountWhenNoAlternative("model_unavailable") {
		t.Fatal("expected model_unavailable to retry the current account when no alternative exists")
	}
}
