package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/alerting"
)

// TestSuccessTarget_ComesFromTheEngine pins why the number exists: the page shows
// a success rate with no stated target, so the threshold the alerts fire on is
// published next to it. It must come from the engine, not a literal here, or the
// two would drift apart.
func TestSuccessTarget_ComesFromTheEngine(t *testing.T) {
	engine := alerting.NewEngine(alerting.DefaultRules(), nil)
	target, source := successTarget(engine)
	if target != 0.9 {
		t.Fatalf("successTarget() = %v, want 0.9", target)
	}
	if source == "" {
		t.Fatal("success_target_source must name where the target comes from")
	}

	custom := alerting.DefaultRules()
	custom.SuccessRateWarning = 0.8
	if got, _ := successTarget(alerting.NewEngine(custom, nil)); got != 0.8 {
		t.Fatalf("successTarget() with a custom policy = %v, want 0.8", got)
	}
}

// TestSuccessTarget_NilAndZeroValueFallBackToTheShippedPolicy covers the two ways
// the threshold can be missing: no engine wired at all (aggregation disabled,
// tests) and a Rules zero value. Both must yield 0.9 — a returned 0 would make the
// page print "目标 0.0%" and look broken.
func TestSuccessTarget_NilAndZeroValueFallBackToTheShippedPolicy(t *testing.T) {
	got, source := successTarget(nil)
	if got != 0.9 {
		t.Fatalf("successTarget(nil) = %v, want the default 0.9", got)
	}
	if source == "" {
		t.Fatal("success_target_source must still be present for a nil engine")
	}

	zero := alerting.NewEngine(alerting.Rules{}, nil)
	if got, _ := successTarget(zero); got != 0.9 {
		t.Fatalf("successTarget() on a zero-value Rules = %v, want 0.9", got)
	}
	if got := successTargetCritical(zero); got != 0.5 {
		t.Fatalf("successTargetCritical() on a zero-value Rules = %v, want 0.5", got)
	}
	if got := successTargetCritical(nil); got != 0.5 {
		t.Fatalf("successTargetCritical(nil) = %v, want 0.5", got)
	}
}

// TestSuccessTarget_SourceIsRenderable keeps the provenance string a UI can print
// as-is: short, and free of the characters that would need escaping in JSON.
func TestSuccessTarget_SourceIsRenderable(t *testing.T) {
	_, source := successTarget(nil)
	if len([]rune(source)) > 40 {
		t.Fatalf("success_target_source too long for a label: %q", source)
	}
	for _, forbidden := range []string{"\"", "\\", "\n", "\t", "<", ">"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("success_target_source %q contains %q", source, forbidden)
		}
	}
}

// TestHandleOpsOverview_PublishesTheTargetOnTheUnavailableBranch is the early
// return: when aggregation is off the handler answers and leaves, so the target
// has to be in the payload before that. The page still shows a success rate in
// that state and must be able to say what it is measured against.
func TestHandleOpsOverview_PublishesTheTargetOnTheUnavailableBranch(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&API{}).HandleOpsOverview(recorder, httptest.NewRequest(http.MethodGet, "/api/ops/overview", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("overview is not JSON: %v (%s)", err, recorder.Body.String())
	}
	if available, _ := payload["available"].(bool); available {
		t.Fatalf("expected the unavailable branch, got available = %v", payload["available"])
	}
	target, ok := payload["success_target"].(float64)
	if !ok {
		t.Fatalf("success_target missing from the unavailable payload: %v", payload)
	}
	if target != 0.9 {
		t.Fatalf("success_target = %v, want the default 0.9", target)
	}
	if _, ok := payload["success_target_source"].(string); !ok {
		t.Fatalf("success_target_source missing: %v", payload)
	}
}
