package grok

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLocalErrorKeepsItsStatusAndMessage is the guard for the classification rule.
//
// writeGrokUpstreamError decides between "an upstream failed" and "the caller sent
// something we reject" by inspecting the error text. The status probe used to match
// a bare "status=", so any local error that happened to mention a status was
// reclassified as an upstream failure: it answered 5xx for a 4xx condition and
// replaced the local message with a generic sentence.
func TestLocalErrorKeepsItsStatusAndMessage(t *testing.T) {
	local := []string{
		"job status=404 not found in store",
		"account status=402 parked",
		"invalid request: expected status=200 payload",
		"store=true requires stream=false for this provider",
		"aspect_ratio is not supported",
	}
	for _, message := range local {
		rec := httptest.NewRecorder()
		writeGrokUpstreamError(rec, errors.New(message))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400 for a local error", message, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), message) {
			t.Errorf("%q: the local message was replaced: %s", message, rec.Body.String())
		}
	}
}

// TestUpstreamErrorIsStillSanitized pins the other half: a genuine upstream failure
// is still classified as one, answered 5xx, and stripped of upstream detail.
func TestUpstreamErrorIsStillSanitized(t *testing.T) {
	upstream := []string{
		"grok upstream status=502 body=bad gateway from xai",
		"grok cli upstream status=403 body=forbidden",
	}
	for _, message := range upstream {
		rec := httptest.NewRecorder()
		writeGrokUpstreamError(rec, errors.New(message))
		// A credential-class failure is answered 503 (grok2api does the same: the
		// caller's own key is fine, the account pool needs operator action); any
		// other upstream failure carries the status for its category.
		if rec.Code < 400 {
			t.Errorf("%q: status = %d, want an error for an upstream failure", message, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "body=") {
			t.Errorf("%q: upstream body leaked to the client: %s", message, rec.Body.String())
		}
	}
}
