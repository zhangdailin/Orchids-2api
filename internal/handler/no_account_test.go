package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apperrors "orchids-api/internal/errors"
)

// The pool's note says why the channel cannot serve a request, and each reason
// needs its own answer. These are the production shapes: the first is the
// WorkBuddy pool the client saw as "no enabled accounts available for channel:
// workbuddy" with a 503, after every account that could serve the requested model
// had gone into a per-model cooldown.
func TestClassifyPoolExhaustion_SelectorReasonsPickTheAnswer(t *testing.T) {
	cases := []struct {
		name         string
		selectErr    error
		lastErr      string
		wantCategory string
		wantStatus   int
		wantMessage  string
	}{
		{
			name:         "every account is cooling down for the requested model",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are cooling down for the requested model)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "the requested model is cooling down on this channel",
		},
		{
			name:         "the whole pool is rate limited",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are rate-limited or cooling down)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "all available accounts for this channel are currently rate-limited",
		},
		{
			name:         "every account has spent its allowance",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts have exhausted their allowance)"),
			wantCategory: "quota_exhausted",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "has exhausted its allowance",
		},
		{
			name:         "every account is busy with other requests",
			selectErr:    errors.New("no enabled accounts available for channel: warp (all matching accounts are at their concurrency limit)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "busy with other requests",
		},
		{
			name:         "the pool cannot route the requested model",
			selectErr:    errors.New("no enabled accounts available for channel: warp (model gpt-5-6-sol-low is not available in the current Warp account pool)"),
			wantCategory: "model_unavailable",
			wantStatus:   http.StatusNotFound,
			wantMessage:  "is not available on this channel's accounts",
		},
		{
			name:         "the upstream error carries the cause",
			selectErr:    errors.New("no enabled accounts available for channel: puter"),
			lastErr:      `upstream API error: status=429, body={"code":"rate-limited"}`,
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "currently rate-limited",
		},
		{
			name:         "an exhausted allowance in the upstream error wins over a busy pool",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are cooling down for the requested model)"),
			lastErr:      `workbuddy API error: status=429, message={"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs"}}}`,
			wantCategory: "quota_exhausted",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "has exhausted its allowance",
		},
		{
			name:         "Qoder's model rate limit keeps naming the channel",
			selectErr:    errors.New("no enabled accounts available for channel: qoder"),
			lastErr:      "qoder model rate limited",
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "the requested Qoder model is temporarily rate-limited",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := classifyPoolExhaustion(tc.selectErr, tc.lastErr)
			if out.category != tc.wantCategory {
				t.Fatalf("category = %q, want %q", out.category, tc.wantCategory)
			}
			if got := apperrors.StatusForCategory(out.category); got != tc.wantStatus {
				t.Fatalf("status = %d, want %d", got, tc.wantStatus)
			}
			if !strings.Contains(out.message, tc.wantMessage) {
				t.Fatalf("message = %q, want it to contain %q", out.message, tc.wantMessage)
			}
		})
	}
}

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
	selectErr := errors.New("no enabled accounts available for channel: warp")
	rec := httptest.NewRecorder()

	writePoolExhaustion(rec, classifyPoolExhaustion(selectErr, ""))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no account in this channel can serve the request") {
		t.Fatalf("body = %s, want the pool-unavailable answer", body)
	}
	if strings.Contains(body, "no enabled accounts available for channel") {
		t.Fatalf("selector detail leaked into the response: %s", body)
	}
}
