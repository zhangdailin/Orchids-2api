package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// TestHandleKeysCreateBillingLimitEnforcesReservation is the end-to-end admin
// contract: a limit set through the API is what the request path reserves
// against, without any cache or restart in between.
func TestHandleKeysCreateBillingLimitEnforcesReservation(t *testing.T) {
	s, mini := newTestStore(t, "api-keys-billing:")
	defer mini.Close()
	defer s.Close()
	a := New(s, "admin", "pass", &config.Config{})
	ctx := t.Context()

	const limit int64 = 5_000_000_000 // 0.50 USD
	createReq := httptest.NewRequest(http.MethodPost, "/api/keys",
		strings.NewReader(fmt.Sprintf(`{"name":"metered","billing_limit_usd_ticks":%d}`, limit)))
	createRec := httptest.NewRecorder()
	a.HandleKeys(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created CreateKeyResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.BillingLimitUSDTicks != limit {
		t.Fatalf("created limit = %d, want %d", created.BillingLimitUSDTicks, limit)
	}

	// The limit is persisted and visible through the admin list.
	stored, err := s.GetApiKeyByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID() error = %v", err)
	}
	if stored.BillingLimitUSDTicks != limit {
		t.Fatalf("stored limit = %d, want %d", stored.BillingLimitUSDTicks, limit)
	}
	listed, err := s.ListApiKeys(ctx)
	if err != nil || len(listed) != 1 || listed[0].BillingLimitUSDTicks != limit {
		t.Fatalf("ListApiKeys() = %#v, %v", listed, err)
	}

	// Reserving inside the limit succeeds; the request that would cross it does
	// not.
	ok, err := s.ReserveApiKeyBilling(ctx, created.ID, "event-a", limit-1, time.Now().UTC().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("reservation inside the limit = %v, %v", ok, err)
	}
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-b", 2, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReserveApiKeyBilling() error = %v", err)
	}
	if ok {
		t.Fatal("a reservation crossing the limit must be refused")
	}
	// Idempotency: the same event id and amount is the same request, not a
	// second hold.
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-a", limit-1, time.Now().UTC().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("idempotent re-reserve = %v, %v", ok, err)
	}

	// Settling the request books its cost, and the budget is then exhausted.
	if err := s.SettleApiKeyBilling(ctx, created.ID, "event-a", limit-1); err != nil {
		t.Fatalf("SettleApiKeyBilling() error = %v", err)
	}
	ok, err = s.ReserveApiKeyBilling(ctx, created.ID, "event-c", 2, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("ReserveApiKeyBilling() error = %v", err)
	}
	if ok {
		t.Fatal("a settled key with no headroom must refuse further reservations")
	}
	// The remaining single tick is still spendable: the limit is a ceiling, not
	// an exclusive bound.
	if ok, err := s.ReserveApiKeyBilling(ctx, created.ID, "event-c", 1, time.Now().UTC().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("reservation up to the exact limit = %v, %v", ok, err)
	}
}

// TestHandleKeyBillingLimitUpdateTakesEffect checks the PATCH path, including
// that a billing limit alone counts as a policy change.
func TestHandleKeyBillingLimitUpdateTakesEffect(t *testing.T) {
	s, mini := newTestStore(t, "api-keys-billing-patch:")
	defer mini.Close()
	defer s.Close()
	a := New(s, "admin", "pass", &config.Config{})
	ctx := t.Context()

	key := &store.ApiKey{Name: "unlimited", KeyHash: "hash-unlimited", Enabled: true}
	if err := s.CreateApiKey(ctx, key); err != nil {
		t.Fatalf("CreateApiKey() error = %v", err)
	}
	if ok, err := s.ReserveApiKeyBilling(ctx, key.ID, "event-unlimited", 1_000_000, time.Now().UTC().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("unlimited reservation = %v, %v", ok, err)
	}
	if _, err := s.ReleaseApiKeyBilling(ctx, key.ID, "event-unlimited"); err != nil {
		t.Fatalf("ReleaseApiKeyBilling() error = %v", err)
	}

	// A PATCH carrying only the billing limit must be accepted.
	patchReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/keys/%d", key.ID),
		strings.NewReader(`{"billing_limit_usd_ticks":1000}`))
	patchRec := httptest.NewRecorder()
	a.HandleKeyByID(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", patchRec.Code, patchRec.Body.String())
	}
	var updated store.ApiKey
	if err := json.Unmarshal(patchRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if updated.BillingLimitUSDTicks != 1000 {
		t.Fatalf("patched limit = %d, want 1000", updated.BillingLimitUSDTicks)
	}
	if ok, err := s.ReserveApiKeyBilling(ctx, key.ID, "event-over", 1001, time.Now().UTC().Add(time.Hour)); err != nil || ok {
		t.Fatalf("reservation above the patched limit = %v, %v", ok, err)
	}
	if ok, err := s.ReserveApiKeyBilling(ctx, key.ID, "event-fits", 1000, time.Now().UTC().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("reservation at the patched limit = %v, %v", ok, err)
	}

	// A PATCH with no policy field at all is still rejected.
	emptyReq := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/keys/%d", key.ID), strings.NewReader(`{}`))
	emptyRec := httptest.NewRecorder()
	a.HandleKeyByID(emptyRec, emptyReq)
	if emptyRec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch status=%d body=%s", emptyRec.Code, emptyRec.Body.String())
	}
}

// TestHandleKeysRejectsInvalidBillingLimits keeps a negative or overflow-prone
// budget out of the ledger.
func TestHandleKeysRejectsInvalidBillingLimits(t *testing.T) {
	s, mini := newTestStore(t, "api-keys-billing-invalid:")
	defer mini.Close()
	defer s.Close()
	a := New(s, "admin", "pass", &config.Config{})

	for _, body := range []string{
		`{"name":"negative","billing_limit_usd_ticks":-1}`,
		`{"name":"overflow","billing_limit_usd_ticks":9000000000000001}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
		rec := httptest.NewRecorder()
		a.HandleKeys(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}
