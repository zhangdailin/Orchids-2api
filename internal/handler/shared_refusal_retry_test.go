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

// TestSharedRefusalWaitRampsUpToTheHint pins the latency shape. The upstream's
// Retry-After on a queue refusal is the window's worst case; paying it in full on
// the first retry made every affected request wait the whole window even when the
// queue had already cleared. The schedule probes early and converges on the hint.
func TestSharedRefusalWaitRampsUpToTheHint(t *testing.T) {
	t.Parallel()

	hint := 30 * time.Second
	previous := time.Duration(0)
	cumulative := time.Duration(0)
	for retry := 1; retry <= 3; retry++ {
		wait := sharedRefusalWait(hint, retry)
		if wait <= previous {
			t.Fatalf("retry %d wait %v did not grow past %v", retry, wait, previous)
		}
		if wait > hint {
			t.Fatalf("retry %d wait %v exceeds the hint %v", retry, wait, hint)
		}
		previous = wait
		cumulative += wait
	}
	// The first probe must be cheap: that is where the latency win comes from.
	if first := sharedRefusalWait(hint, 1); first > hint/4 {
		t.Errorf("first probe = %v, want well under the hint %v", first, hint)
	}
	// Three retries must still cover roughly the whole window, or a slow queue
	// would never be waited out.
	if cumulative < hint {
		t.Errorf("three retries cover %v, want at least the %v hint", cumulative, hint)
	}
	// Beyond the schedule the hint is honoured, and every value stays bounded.
	for retry := 4; retry <= 8; retry++ {
		if wait := sharedRefusalWait(hint, retry); wait != hint {
			t.Errorf("retry %d wait = %v, want the hint %v", retry, wait, hint)
		}
	}
}

// TestSharedRefusalWaitEdges covers the degenerate hints: no hint means no extra
// wait, and a tiny hint still gets a floor so the first probe cannot turn into a
// tight retry loop against the queue.
func TestSharedRefusalWaitEdges(t *testing.T) {
	t.Parallel()

	if got := sharedRefusalWait(0, 1); got != 0 {
		t.Errorf("zero hint = %v, want 0", got)
	}
	if got := sharedRefusalWait(-time.Second, 1); got != 0 {
		t.Errorf("negative hint = %v, want 0", got)
	}
	if got := sharedRefusalWait(2*time.Second, 1); got != time.Second {
		t.Errorf("2s hint first probe = %v, want the 1s floor", got)
	}
	if got := sharedRefusalWait(500*time.Millisecond, 3); got != 500*time.Millisecond {
		t.Errorf("sub-floor hint = %v, want it honoured rather than raised above itself", got)
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
