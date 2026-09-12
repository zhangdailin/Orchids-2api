package accountpolicy

import (
	"errors"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

// TestRetryable_MatchesTheSharedUpstreamClassification pins the single rule the
// request path and the scheduler now share. Before this the handler decided
// retries from category strings while the scheduler decided state from the
// policy, so the same error could be "retryable" in one entrance and "cooling
// down for ten minutes" in the other.
func TestRetryable_MatchesTheSharedUpstreamClassification(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"401: unauthenticated", true},
		{"429: too many requests", true},
		{"reading upstream body: context canceled", false},
		{"502: bad gateway", true},
	}
	for _, tc := range cases {
		err := errors.New(tc.message)
		class := apperrors.ClassifyUpstreamError(tc.message)
		if got := Retryable(err); got != tc.want {
			t.Fatalf("Retryable(%q) = %v, want %v", tc.message, got, tc.want)
		}
		if got, want := Retryable(err), class.Retryable; got != want {
			t.Fatalf("Retryable(%q) = %v but the shared classification says %v", tc.message, got, want)
		}
	}
	if Retryable(nil) {
		t.Fatal("a nil error is not retryable")
	}
}

// TestCancelled_IsItsOwnVerdict keeps "the caller went away" from being reported
// as an upstream fault or cooling the account down.
func TestCancelled_IsItsOwnVerdict(t *testing.T) {
	if !Cancelled(errors.New("context canceled")) {
		t.Fatal("a cancelled request must be recognised")
	}
	if Cancelled(errors.New("429: too many requests")) {
		t.Fatal("a rate limit is not a cancellation")
	}
	if Cancelled(nil) {
		t.Fatal("nil is not a cancellation")
	}
}

// TestClassify_VerdictMatchesTheSharedRetryRule makes the two systems provably
// agree: whatever the policy says about retrying is what the shared
// classification says, for every verdict shape.
func TestClassify_VerdictMatchesTheSharedRetryRule(t *testing.T) {
	acc := &store.Account{AccountType: "grok"}
	messages := []string{
		"401: grok session unauthenticated",
		"403: forbidden",
		"402: out of credits",
		"429: too many requests",
		"404: model is not found",
		"context canceled",
		"502: bad gateway",
	}
	for _, message := range messages {
		err := errors.New(message)
		verdict := Classify(acc, err, "grok-4.6")
		if verdict.Retryable != Retryable(err) {
			t.Fatalf("%q: verdict.Retryable=%v but Retryable()=%v", message, verdict.Retryable, Retryable(err))
		}
	}
}
