package handler

import (
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
)

// The exact status_message stored for production Qoder account 22.
const sharedRefusalMessage = `qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p3\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"waitTime\":30}"}`

// TestSharedRefusalClassIsRecognised pins the signal the retry loop uses. A
// shared refusal is retried on the account already held; anything the classifier
// would rotate away from must not match, or the wait would be spent re-queueing
// behind the same wall.
func TestSharedRefusalClassIsRecognised(t *testing.T) {
	t.Parallel()

	for name, message := range map[string]string{
		"production message":   sharedRefusalMessage,
		"classified busy form": "qoder gateway is busy: serviceAvailable=false retryAfterSeconds=29",
		"upstream pool":        "qoder API error: the available upstream accounts are rate-limited",
	} {
		t.Run(name, func(t *testing.T) {
			if !isSharedUpstreamRefusalClass(apperrors.ClassifyUpstreamError(message)) {
				t.Errorf("%q was not recognised as a shared refusal", message)
			}
		})
	}

	// An account-scoped throttle keeps rotating: a different account can serve it,
	// so it must not take the wait-and-retry-same-account path.
	for name, message := range map[string]string{
		"plain 429":       "upstream API error: status=429, too many requests",
		"cline prose cap": "cline inference cap reached: Try again in 17h 59m",
	} {
		t.Run(name, func(t *testing.T) {
			if isSharedUpstreamRefusalClass(apperrors.ClassifyUpstreamError(message)) {
				t.Errorf("%q was treated as shared; it must still rotate", message)
			}
		})
	}
}

// TestSharedRefusalJitterIsBounded pins that the de-synchronising delay stays a
// fraction of the upstream's own window: it may spread waiters, never displace
// the recovery time the upstream asked for by more than a fifth (or five
// seconds, whichever is smaller).
func TestSharedRefusalJitterIsBounded(t *testing.T) {
	t.Parallel()

	for _, delay := range []time.Duration{0, -time.Second, 100 * time.Millisecond, time.Second, 30 * time.Second, time.Hour} {
		limit := delay / 5
		if limit > 5*time.Second {
			limit = 5 * time.Second
		}
		for i := 0; i < 200; i++ {
			got := sharedRefusalJitter(delay)
			if got < 0 {
				t.Fatalf("jitter(%v) = %v, want non-negative", delay, got)
			}
			if limit <= 0 {
				if got != 0 {
					t.Fatalf("jitter(%v) = %v, want 0 when the wait has no room", delay, got)
				}
				continue
			}
			if got >= limit {
				t.Fatalf("jitter(%v) = %v, want < %v", delay, got, limit)
			}
		}
	}
}

// TestSharedRefusalWaitIsCappedByTheHandler documents that the wait the retry
// loop actually spends is the handler's capped reading of the hint, so a shared
// refusal cannot park a request goroutine on an absurd upstream value.
func TestSharedRefusalWaitIsCappedByTheHandler(t *testing.T) {
	t.Parallel()

	if got := upstreamRetryAfter(hintedRetryError{delay: 30 * time.Second}); got != 30*time.Second {
		t.Fatalf("a 30s shared window = %v, want it honoured", got)
	}
	if got := upstreamRetryAfter(hintedRetryError{delay: 4 * time.Hour}); got != 30*time.Second {
		t.Fatalf("an absurd window = %v, want the 30s cap", got)
	}
}
