package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/audit"
	"orchids-api/internal/pricing"
)

// stubBillingLedger is an in-memory stand-in for the Redis ledger. It applies
// the same limit rule so the middleware's decision is observable without Redis.
type stubBillingLedger struct {
	limit      int64
	used       int64
	held       map[string]int64
	reserveErr error
	settleErr  error
	settles    []int64
	releases   []string
	// ctx captures the reserved request context so a test can settle outside the
	// handler that took the hold.
	ctx context.Context
}

func (s *stubBillingLedger) ReserveApiKeyBilling(_ context.Context, _ int64, eventID string, amount int64, _ time.Time) (bool, error) {
	if s.reserveErr != nil {
		return false, s.reserveErr
	}
	if s.held == nil {
		s.held = map[string]int64{}
	}
	var live int64
	for _, held := range s.held {
		live += held
	}
	if s.limit > 0 && s.used+live+amount > s.limit {
		return false, nil
	}
	s.held[eventID] = amount
	return true, nil
}

func (s *stubBillingLedger) SettleApiKeyBilling(_ context.Context, _ int64, eventID string, amount int64) error {
	if s.settleErr != nil {
		return s.settleErr
	}
	delete(s.held, eventID)
	s.used += amount
	s.settles = append(s.settles, amount)
	return nil
}

func (s *stubBillingLedger) ReleaseApiKeyBilling(_ context.Context, _ int64, eventID string) (bool, error) {
	if _, ok := s.held[eventID]; !ok {
		return false, nil
	}
	delete(s.held, eventID)
	s.releases = append(s.releases, eventID)
	return true, nil
}

func billingRequest(method, path, body string, principal *APIKeyPrincipal, requestID string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := context.WithValue(request.Context(), requestIDKey{}, requestID)
	ctx = context.WithValue(ctx, apiKeyPrincipalContextKey{}, principal)
	return request.WithContext(ctx)
}

const billingChatBody = `{"model":"grok-4.6","max_tokens":512,"messages":[{"role":"user","content":"hello"}]}`

// TestAPIKeyBillingReservationSettles covers the happy path: the hold exists for
// the handler, the handler's body is intact, and settling books the real cost
// instead of leaving the hold behind.
func TestAPIKeyBillingReservationSettles(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 7, BillingLimitUSDTicks: 1_000_000_000_000}

	var (
		seenBody   string
		seenHeld   *BillingReservation
		settled    pricing.Result
		settleOK   bool
		handlerRun bool
	)
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
		raw := make([]byte, 0, len(billingChatBody))
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			raw = append(raw, buf[:n]...)
			if err != nil {
				break
			}
		}
		seenBody = string(raw)
		seenHeld = BillingReservationFrom(r.Context())
		settled, settleOK = SettleAPIKeyBilling(r.Context(), ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/v1/chat/completions", billingChatBody, principal, "req-1"))

	if !handlerRun {
		t.Fatal("handler must run for a reservation that fits")
	}
	if seenBody != billingChatBody {
		t.Fatalf("handler body = %q, want the original body", seenBody)
	}
	if seenHeld == nil || seenHeld.KeyID != 7 || seenHeld.EventID != "req-1" || seenHeld.Amount <= 0 {
		t.Fatalf("reservation in context = %#v", seenHeld)
	}
	if !settleOK || settled.Model != "grok-4.6" {
		t.Fatalf("settle = %#v, %v", settled, settleOK)
	}
	// 1000 input at 20000 ticks + 500 output at 60000 ticks.
	if want := int64(1000*20000 + 500*60000); settled.CostInUSDTicks != want {
		t.Fatalf("cost = %d, want %d", settled.CostInUSDTicks, want)
	}
	if len(ledger.settles) != 1 || ledger.settles[0] != settled.CostInUSDTicks || ledger.used != settled.CostInUSDTicks {
		t.Fatalf("ledger after settle = %#v", ledger)
	}
	if len(ledger.held) != 0 {
		t.Fatalf("hold survived settlement: %#v", ledger.held)
	}
	if len(ledger.releases) != 0 {
		t.Fatalf("a settled request was also released: %#v", ledger.releases)
	}
}

// TestAPIKeyBillingReservationReleasesUnsettledRequest checks the deferred
// cleanup: a request that never settled must not keep holding budget.
func TestAPIKeyBillingReservationReleasesUnsettledRequest(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 9, BillingLimitUSDTicks: 1_000_000_000_000}

	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/v1/messages", billingChatBody, principal, "req-release"))

	if len(ledger.releases) != 1 || ledger.releases[0] != "req-release" {
		t.Fatalf("releases = %#v", ledger.releases)
	}
	if ledger.used != 0 || len(ledger.held) != 0 {
		t.Fatalf("ledger after release = %#v", ledger)
	}
}

// TestAPIKeyBillingReservationRefusesOverLimit pins the 402 contract and that a
// refused request never reaches its handler.
func TestAPIKeyBillingReservationRefusesOverLimit(t *testing.T) {
	ledger := &stubBillingLedger{limit: 100}
	principal := &APIKeyPrincipal{ID: 11, BillingLimitUSDTicks: 100}

	handlerRun := false
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/v1/chat/completions", billingChatBody, principal, "req-over"))

	if handlerRun {
		t.Fatal("a refused reservation must not call the handler")
	}
	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing WWW-Authenticate header")
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("error envelope is not JSON: %v (%s)", err, recorder.Body.String())
	}
	if envelope.Error.Code != "billing_limit_exceeded" || envelope.Error.Message != "API key billing limit exceeded" {
		t.Fatalf("error envelope = %#v", envelope.Error)
	}
	if envelope.Error.Type != "insufficient_quota" {
		t.Fatalf("error type = %q, want insufficient_quota", envelope.Error.Type)
	}
	if len(ledger.held) != 0 {
		t.Fatalf("refused reservation was recorded: %#v", ledger.held)
	}
}

// TestAPIKeyBillingReservationFailsClosedOnLedgerError keeps a Redis outage from
// turning a budgeted key into an unmetered one.
func TestAPIKeyBillingReservationFailsClosedOnLedgerError(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000, reserveErr: context.DeadlineExceeded}
	principal := &APIKeyPrincipal{ID: 12, BillingLimitUSDTicks: 1_000_000_000_000}

	handlerRun := false
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		handlerRun = true
	}, ledger, time.Minute)

	recorder := httptest.NewRecorder()
	handler(recorder, billingRequest(http.MethodPost, "/v1/chat/completions", billingChatBody, principal, "req-err"))

	if handlerRun {
		t.Fatal("a failing ledger must not let the request through unmetered")
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

// TestAPIKeyBillingReservationSkipsUnbilledRequests keeps the middleware off
// every request that has nothing to price.
func TestAPIKeyBillingReservationSkipsUnbilledRequests(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		body      string
		principal *APIKeyPrincipal
	}{
		{"unlimited key", http.MethodPost, "/v1/chat/completions", billingChatBody, &APIKeyPrincipal{ID: 1}},
		{"get", http.MethodGet, "/v1/chat/completions", "", &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"empty body", http.MethodPost, "/v1/chat/completions", "", &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"non inference path", http.MethodPost, "/v1/models", `{"model":"grok-4.6"}`, &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"unpriced model", http.MethodPost, "/v1/chat/completions", `{"model":"gpt-5"}`, &APIKeyPrincipal{ID: 1, BillingLimitUSDTicks: 10}},
		{"no principal", http.MethodPost, "/v1/chat/completions", billingChatBody, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &stubBillingLedger{limit: 10}
			handlerRun := false
			handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
				handlerRun = true
			}, ledger, time.Minute)

			recorder := httptest.NewRecorder()
			handler(recorder, billingRequest(tc.method, tc.path, tc.body, tc.principal, "req-skip"))

			if !handlerRun {
				t.Fatal("handler must still run")
			}
			if len(ledger.held) != 0 || len(ledger.settles) != 0 || len(ledger.releases) != 0 {
				t.Fatalf("ledger was touched: %#v", ledger)
			}
		})
	}
}

// TestAPIKeyBillingReservationReleasesAfterClientDisconnect keeps the cleanup
// working when the request context is already cancelled.
func TestAPIKeyBillingReservationReleasesAfterClientDisconnect(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 21, BillingLimitUSDTicks: 1_000_000_000_000}

	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		// The client disappears mid-request, which cancels r.Context().
		cancel := r.Context().Value(cancelKey{})
		if fn, ok := cancel.(context.CancelFunc); ok {
			fn()
		}
	}, ledger, time.Minute)

	request := billingRequest(http.MethodPost, "/v1/chat/completions", billingChatBody, principal, "req-disconnect")
	ctx, cancel := context.WithCancel(request.Context())
	ctx = context.WithValue(ctx, cancelKey{}, cancel)
	handler(httptest.NewRecorder(), request.WithContext(ctx))

	if len(ledger.releases) != 1 {
		t.Fatalf("releases = %#v, want the cancelled request to release its hold", ledger.releases)
	}
}

type cancelKey struct{}

// TestAPIKeyBillingReservationRestoresOversizedBody checks that a body past the
// estimation cap still reaches the handler byte for byte.
func TestAPIKeyBillingReservationRestoresOversizedBody(t *testing.T) {
	ledger := &stubBillingLedger{limit: 1_000_000_000_000}
	principal := &APIKeyPrincipal{ID: 31, BillingLimitUSDTicks: 1_000_000_000_000}

	var size int
	handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
		raw, err := readAllBody(r)
		if err != nil {
			t.Errorf("handler could not read the body: %v", err)
		}
		size = len(raw)
		if res := BillingReservationFrom(r.Context()); res == nil {
			t.Error("oversized body was not reserved")
		}
		// Settle so the successful reservation stays observable after the
		// middleware's deferred cleanup.
		SettleAPIKeyBilling(r.Context(), ledger, "grok-4.6", audit.UsageSourceUpstream, 10, 0, 10)
	}, ledger, time.Minute)

	body := `{"model":"grok-4.6","max_tokens":8,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", maxBillingBodyBytes+1024) + `"}]}`
	handler(httptest.NewRecorder(), billingRequest(http.MethodPost, "/v1/chat/completions", body, principal, "req-big"))

	if size != len(body) {
		t.Fatalf("handler saw %d bytes, want %d", size, len(body))
	}
	if len(ledger.settles) != 1 {
		t.Fatalf("oversized body was not reserved and settled: %#v", ledger)
	}
}

func readAllBody(r *http.Request) ([]byte, error) {
	var raw bytes.Buffer
	_, err := raw.ReadFrom(r.Body)
	return raw.Bytes(), err
}

// TestSettleAPIKeyBillingRules pins the pricing policy in one place.
func TestSettleAPIKeyBillingRules(t *testing.T) {
	principal := &APIKeyPrincipal{ID: 41, BillingLimitUSDTicks: 1_000_000_000_000}
	reserved := func(t *testing.T, ledger *stubBillingLedger) context.Context {
		t.Helper()
		request := billingRequest(http.MethodPost, "/v1/chat/completions", billingChatBody, principal, "req-rules")
		handler := APIKeyBillingReservation(func(w http.ResponseWriter, r *http.Request) {
			// Hand the reserved context back to the test through the ledger.
			ledger.ctx = r.Context()
		}, ledger, time.Minute)
		handler(httptest.NewRecorder(), request)
		return ledger.ctx
	}

	t.Run("estimated usage is not billed", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		if _, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceEstimated, 1000, 0, 500); priced {
			t.Fatal("an estimated row must not be priced")
		}
		if len(ledger.settles) != 0 {
			t.Fatalf("settles = %#v", ledger.settles)
		}
	})

	t.Run("unpriced model", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		if _, priced := SettleAPIKeyBilling(ctx, ledger, "gpt-5", audit.UsageSourceUpstream, 1000, 0, 500); priced {
			t.Fatal("an unpriced model must not be priced")
		}
		if len(ledger.settles) != 0 {
			t.Fatalf("settles = %#v", ledger.settles)
		}
	})

	t.Run("idempotent per event id", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		ctx := reserved(t, ledger)
		first, _ := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		second, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		if !priced || first != second {
			t.Fatalf("first=%#v second=%#v priced=%v", first, second, priced)
		}
		if len(ledger.settles) != 1 {
			t.Fatalf("settles = %#v, want exactly one charge", ledger.settles)
		}
	})

	t.Run("without a reservation the cost is still reported", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		result, priced := SettleAPIKeyBilling(context.Background(), ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		if !priced || result.Model != "grok-4.6" {
			t.Fatalf("result=%#v priced=%v", result, priced)
		}
		if len(ledger.settles) != 0 || ledger.used != 0 {
			t.Fatalf("unreserved request was charged: %#v", ledger)
		}
	})

	t.Run("nil settler falls back to the wired ledger", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000}
		SetAPIKeyBillingStore(ledger)
		t.Cleanup(func() { SetAPIKeyBillingStore(nil) })
		ctx := reserved(t, ledger)
		if _, priced := SettleAPIKeyBilling(ctx, nil, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500); !priced {
			t.Fatal("a wired ledger must be used")
		}
		if len(ledger.settles) != 1 {
			t.Fatalf("settles = %#v", ledger.settles)
		}
	})

	t.Run("settle failure keeps the audit cost", func(t *testing.T) {
		ledger := &stubBillingLedger{limit: 1_000_000_000_000, settleErr: context.DeadlineExceeded}
		ctx := reserved(t, ledger)
		result, priced := SettleAPIKeyBilling(ctx, ledger, "grok-4.6", audit.UsageSourceUpstream, 1000, 0, 500)
		if !priced || result.CostInUSDTicks == 0 {
			t.Fatalf("result=%#v priced=%v", result, priced)
		}
		// The hold stays claimed so a retry cannot charge twice.
		if res := BillingReservationFrom(ctx); res == nil || !res.Settled() {
			t.Fatal("a failed settlement must still claim the reservation")
		}
	})
}

// TestBillingReservationConcurrentSettleIsIdempotent covers the streaming path,
// where a settle call may race the request's own cleanup.
func TestBillingReservationConcurrentSettleIsIdempotent(t *testing.T) {
	held := &BillingReservation{KeyID: 1, EventID: "event", Amount: 10}
	const workers = 8
	claimed := make(chan bool, workers)
	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			_, first := held.claim(pricing.Result{Model: "grok-4.6", CostInUSDTicks: 5})
			claimed <- first
			<-done
		}()
	}
	winners := 0
	for i := 0; i < workers; i++ {
		if <-claimed {
			winners++
		}
	}
	close(done)
	if winners != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", winners)
	}
}

func TestBillingRequestPathMatching(t *testing.T) {
	priced := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/grok/v1/messages", "/workbuddy/v1/chat/completions"}
	for _, path := range priced {
		if !billingRequestPath(path) {
			t.Fatalf("billingRequestPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/v1/models", "/api/keys", "/v1/tts", "/v1/images/generations", "/"} {
		if billingRequestPath(path) {
			t.Fatalf("billingRequestPath(%q) = true, want false", path)
		}
	}
}

// A media request hands in its own price, and the same claim-once rule applies so
// a retry cannot charge twice.
func TestSettleAPIKeyBillingResultBooksAnExplicitPrice(t *testing.T) {
	stub := &stubBillingLedger{limit: 10_000_000_000, held: map[string]int64{"req_media_1": 1_000_000_000}}
	reservation := &BillingReservation{KeyID: 7, EventID: "req_media_1", Amount: 1_000_000_000}
	ctx := WithBillingReservation(context.Background(), reservation)

	price := pricing.Result{Model: "grok-imagine-image", CostInUSDTicks: 200_000_000}
	if !SettleAPIKeyBillingResult(ctx, stub, price) {
		t.Fatal("an explicit price was not booked")
	}
	if len(stub.settles) != 1 || stub.settles[0] != 200_000_000 || stub.used != 200_000_000 {
		t.Fatalf("settles=%v used=%d", stub.settles, stub.used)
	}
	// A second call for the same request charges nothing more.
	SettleAPIKeyBillingResult(ctx, stub, price)
	if len(stub.settles) != 1 || stub.used != 200_000_000 {
		t.Fatalf("the request was charged twice: settles=%v used=%d", stub.settles, stub.used)
	}
	// An unpriced request is neither booked nor an error.
	if SettleAPIKeyBillingResult(ctx, stub, pricing.Result{}) {
		t.Fatal("an unpriced result was booked")
	}
	if len(stub.settles) != 1 {
		t.Fatalf("settles=%v", stub.settles)
	}
	// A key with no hold still defines a price (the audit row carries it), but
	// nothing is booked.
	if !SettleAPIKeyBillingResult(context.Background(), stub, price) || len(stub.settles) != 1 {
		t.Fatalf("an unheld request booked usage: settles=%v", stub.settles)
	}
}
