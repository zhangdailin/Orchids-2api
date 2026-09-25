package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apperrors "orchids-api/internal/errors"
)

// TestWritePoolExhaustion_CapacityProblemIsRetryable is the initial-selection
// contract: a pool that is cooling down is a capacity problem, so the answer is a
// retryable 429 with the cause in the text — not the 503 "overloaded_error" that
// carried the selector's internal note and made a cooldown look like a server
// fault (the shape the WorkBuddy outage was reported to its callers in).
func TestWritePoolExhaustion_CapacityProblemIsRetryable(t *testing.T) {
	selectErr := errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are cooling down for the requested model)")
	rec := httptest.NewRecorder()

	writePoolExhaustion(rec, classifyPoolExhaustion(selectErr, selectErr.Error()))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 for a cooling pool (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "the requested model is cooling down on this channel") {
		t.Fatalf("body = %s, want the model-cooldown answer", body)
	}
	if strings.Contains(body, "no enabled accounts available for channel") {
		t.Fatalf("selector detail leaked into the response: %s", body)
	}
	if strings.Contains(body, "overloaded_error") {
		t.Fatalf("a capacity problem was reported as a server fault: %s", body)
	}
}

// TestWritePoolExhaustion_ResidualCausePointsAtTheOperator covers everything that
// cannot be explained by a cooldown or an allowance: with no accounts at all, or
// accounts the upstream refuses outright, waiting is not the answer and the client
// needs to be told that an operator has to act.
func TestWritePoolExhaustion_ResidualCausePointsAtTheOperator(t *testing.T) {
	selectErr := errors.New("no enabled accounts available for channel: grok")
	rec := httptest.NewRecorder()

	writePoolExhaustion(rec, classifyPoolExhaustion(selectErr, ""))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, apperrors.PoolNoAccountsMessage) {
		t.Fatalf("body = %s, want the pool-unavailable answer", body)
	}
	if strings.Contains(body, "no enabled accounts available for channel") {
		t.Fatalf("selector detail leaked into the response: %s", body)
	}
}
