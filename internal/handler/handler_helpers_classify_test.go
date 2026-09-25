package handler

import (
	"errors"
	"fmt"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
)

type hintedRetryError struct{ delay time.Duration }

func (e hintedRetryError) Error() string             { return "retry later" }
func (e hintedRetryError) RetryAfter() time.Duration { return e.delay }

func TestUpstreamRetryAfterReadsWrappedHint(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", hintedRetryError{delay: 7 * time.Second})
	if got := upstreamRetryAfter(err); got != 7*time.Second {
		t.Fatalf("upstreamRetryAfter() = %v, want 7s", got)
	}
	if got := upstreamRetryAfter(hintedRetryError{delay: time.Minute}); got != 30*time.Second {
		t.Fatalf("upstreamRetryAfter() cap = %v, want 30s", got)
	}
	if got := upstreamRetryAfter(errors.New("plain")); got != 0 {
		t.Fatalf("upstreamRetryAfter(plain) = %v, want 0", got)
	}
}

func TestClassifyUpstreamErrorCreditsExhausted(t *testing.T) {
	t.Parallel()

	errClass := apperrors.ClassifyUpstreamError("workbuddy upstream error: no remaining quota: You have run out of credits.")
	if errClass.Category != "quota_exhausted" {
		t.Fatalf("expected quota_exhausted category, got %q", errClass.Category)
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
