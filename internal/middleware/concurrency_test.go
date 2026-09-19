package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetP95_NotEnoughData(t *testing.T) {
	cl := NewConcurrencyLimiter(1, time.Second, true)
	for i := 0; i < 9; i++ {
		cl.UpdateStats(10 * time.Millisecond)
	}
	if p95 := atomic.LoadInt64(&cl.cachedP95); p95 != 0 {
		t.Fatalf("expected 0 with insufficient samples, got %d", p95)
	}
}

func TestGetP95_Computes(t *testing.T) {
	cl := NewConcurrencyLimiter(1, time.Second, true)
	for i := 0; i < 100; i++ {
		cl.UpdateStats(time.Duration(i+1) * time.Millisecond)
	}

	// Force the 1s throttle to expire so a recalc occurs
	time.Sleep(1100 * time.Millisecond)
	cl.UpdateStats(100 * time.Millisecond)

	p95 := atomic.LoadInt64(&cl.cachedP95)
	if p95 < 90 || p95 > 100 {
		t.Fatalf("expected p95 near top end, got %d", p95)
	}
}

func TestLimiterRejectsImmediatelyWhenBusyWithOpenAIError(t *testing.T) {
	cl := NewConcurrencyLimiter(1, time.Second, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	h := cl.Limit(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})

	go func() {
		defer close(done)
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil))
	}()
	<-entered

	recorder := httptest.NewRecorder()
	start := time.Now()
	h(recorder, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	elapsed := time.Since(start)

	if elapsed >= 100*time.Millisecond {
		t.Fatalf("overloaded request waited %s; want immediate rejection", elapsed)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After=%q want 1", got)
	}
	var envelope struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v; body=%q", err, recorder.Body.String())
	}
	if envelope.Error.Message != "server is overloaded; retry later" ||
		envelope.Error.Type != "server_error" ||
		envelope.Error.Code != "server_overloaded" || envelope.Error.Param != nil {
		t.Fatalf("unexpected OpenAI error envelope: %+v", envelope.Error)
	}
	if got := atomic.LoadInt64(&cl.rejectedReqs); got != 1 {
		t.Fatalf("rejectedReqs=%d want=1", got)
	}

	close(release)
	<-done
}

func TestLimitPreservesExecutionTimeout(t *testing.T) {
	cl := NewConcurrencyLimiter(1, 20*time.Millisecond, false)
	contextErr := make(chan error, 1)
	h := cl.Limit(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		contextErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	h(recorder, httptest.NewRequest(http.MethodGet, "http://x/", nil))
	if got := <-contextErr; got == nil {
		t.Fatal("ordinary limiter did not apply execution timeout")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusNoContent)
	}
}

func TestLimitLongLivedDoesNotInjectExecutionDeadline(t *testing.T) {
	cl := NewConcurrencyLimiter(1, 20*time.Millisecond, false)
	handlerDone := make(chan bool, 1)
	h := cl.LimitLongLived(func(w http.ResponseWriter, r *http.Request) {
		_, hasDeadline := r.Context().Deadline()
		time.Sleep(30 * time.Millisecond)
		handlerDone <- hasDeadline || r.Context().Err() != nil
		w.WriteHeader(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	h(recorder, httptest.NewRequest(http.MethodGet, "http://x/realtime", nil))
	if got := <-handlerDone; got {
		t.Fatal("long-lived limiter injected or triggered an execution deadline")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d want=%d", recorder.Code, http.StatusNoContent)
	}
}
