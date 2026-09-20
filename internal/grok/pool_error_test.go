package grok

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteGrokAccountUnavailable_ClassifiesThePool pins that the Grok handlers
// answer a pool failure the way every other entrance does. They select their own
// sessions, so before this they answered 503 with the pool's own error text: a
// cooling pool reached the caller as a server fault, and the note
// "no enabled accounts available for channel: grok (…)" — which names internal
// state — travelled with it.
func TestWriteGrokAccountUnavailable_ClassifiesThePool(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "cooling down for the requested model",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are cooling down for the requested model)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "the requested model is cooling down on this channel",
		},
		{
			name:        "pool rate limited",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are rate-limited or cooling down)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "all available accounts for this channel are currently rate-limited",
		},
		{
			name:        "allowance exhausted",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts have exhausted their allowance)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "has exhausted its allowance",
		},
		{
			name:        "all accounts busy",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are at their concurrency limit)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "busy with other requests",
		},
		{
			name:        "the pool cannot route the model",
			err:         errors.New("no enabled accounts available for channel: grok (model grok-imagine-video-1.5 is not available in the current Warp account pool)"),
			wantStatus:  http.StatusNotFound,
			wantMessage: "is not available on this channel's accounts",
		},
		{
			name:        "a failure that names no capacity cause stays 503 without its detail",
			err:         errors.New("grok cli account token is empty"),
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: grokResponseAccountUnavailableMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGrokAccountUnavailable(rec, tc.err, "response_account_unavailable", grokResponseAccountUnavailableMessage)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantMessage) {
				t.Fatalf("body = %s, want it to contain %q", body, tc.wantMessage)
			}
			// The pool's note and the underlying error text are diagnostics: they
			// belong in the log, never in a body a client may show to a user.
			for _, leaked := range []string{"no enabled accounts available", "channel: grok", tc.err.Error()} {
				if leaked == tc.wantMessage {
					continue
				}
				if strings.Contains(body, leaked) {
					t.Fatalf("internal detail %q leaked into the response: %s", leaked, body)
				}
			}
		})
	}
}

// TestWriteGrokNoAccountError_ClassifiesTheSameWayAsEveryOtherEntrance covers the
// chat/completions plane: writeGrokNoAccountError used to accept the pool error
// and ignore it, so all eight of its callers answered a cooling pool with 503 and
// a hard-coded sentence. The envelope stays this plane's OpenAI-shaped one; the
// status now follows the cause.
func TestWriteGrokNoAccountError_ClassifiesTheSameWayAsEveryOtherEntrance(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "pool rate limited",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are rate-limited or cooling down)"),
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "all available accounts for this channel are currently rate-limited",
		},
		{
			name:        "allowance exhausted",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts have exhausted their allowance)"),
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "has exhausted its allowance",
		},
		{
			name:        "the pool has no accounts at all",
			err:         errors.New("no enabled accounts available for channel: grok"),
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "server_error",
			wantMessage: grokModelAccountUnavailableMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGrokNoAccountError(rec, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantMessage) {
				t.Fatalf("body = %s, want it to contain %q", body, tc.wantMessage)
			}
			if !strings.Contains(body, `"type":"`+tc.wantType+`"`) {
				t.Fatalf("body = %s, want error type %q", body, tc.wantType)
			}
			if strings.Contains(body, "no enabled accounts available") {
				t.Fatalf("pool note leaked into the response: %s", body)
			}
		})
	}
}

// TestVideoJobFailureMessageKeepsThePoolNoteOutOfTheJob covers the asynchronous
// path: a job is read back with a 200, so whatever is stored is a client-facing
// body. A pool failure is stored as its classified message; every other failure
// keeps its own text, which describes that job rather than the pool.
func TestVideoJobFailureMessageKeepsThePoolNoteOutOfTheJob(t *testing.T) {
	poolErr := errors.New("no enabled accounts available for channel: grok (all matching accounts are cooling down for the requested model)")
	message, fromPool := videoJobFailureMessage(poolErr)
	if !fromPool {
		t.Fatal("a pool failure must be recognised as one")
	}
	if strings.Contains(message, "no enabled accounts available") {
		t.Fatalf("pool note stored on the job: %s", message)
	}
	if !strings.Contains(message, "cooling down on this channel") {
		t.Fatalf("message = %q, want the classified answer", message)
	}

	jobErr := errors.New("grok video render failed: status=500 body=internal")
	message, fromPool = videoJobFailureMessage(jobErr)
	if fromPool {
		t.Fatal("a render failure is not a pool failure")
	}
	if message != jobErr.Error() {
		t.Fatalf("message = %q, want the job's own text", message)
	}

	if message, fromPool = videoJobFailureMessage(nil); message != "" || fromPool {
		t.Fatalf("nil error = (%q, %v), want empty", message, fromPool)
	}
}
