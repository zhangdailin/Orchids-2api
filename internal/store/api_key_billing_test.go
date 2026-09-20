package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newApiKeyBillingStore(t *testing.T, prefix string) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: prefix})
	if err != nil {
		mini.Close()
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

func createBillingKey(t *testing.T, s *Store, limit int64) *ApiKey {
	t.Helper()
	key := &ApiKey{
		Name:                 "billing-" + t.Name(),
		KeyHash:              strings.ReplaceAll(t.Name(), "/", "-"),
		Enabled:              true,
		BillingLimitUSDTicks: limit,
	}
	if err := s.CreateApiKey(context.Background(), key); err != nil {
		t.Fatalf("CreateApiKey() error = %v", err)
	}
	if key.ID == 0 {
		t.Fatal("CreateApiKey() did not assign an id")
	}
	return key
}

func reserve(t *testing.T, s *Store, id int64, eventID string, amount int64, ttl time.Duration) bool {
	t.Helper()
	ok, err := s.ReserveApiKeyBilling(context.Background(), id, eventID, amount, time.Now().UTC().Add(ttl))
	if err != nil {
		t.Fatalf("ReserveApiKeyBilling(%s, %d) error = %v", eventID, amount, err)
	}
	return ok
}

// TestApiKeyBillingUnlimitedKeyNeverBlocks pins the zero-limit contract: a key
// created before billing limits existed must not be rationed.
func TestApiKeyBillingUnlimitedKeyNeverBlocks(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-unlimited:")
	key := createBillingKey(t, s, 0)

	for i, eventID := range []string{"event-a", "event-b", "event-c"} {
		if !reserve(t, s, key.ID, eventID, 1_000_000_000, time.Hour) {
			t.Fatalf("reservation %d of an unlimited key was refused", i)
		}
	}
	got, err := s.GetApiKeyByID(context.Background(), key.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID() error = %v", err)
	}
	if got.BillingUsedUSDTicks != 0 {
		t.Fatalf("used = %d, want 0 before any settlement", got.BillingUsedUSDTicks)
	}
}

// TestApiKeyBillingLimitBlocksExceedingReservation is the core guard: live holds
// plus settled usage may never cross the limit.
func TestApiKeyBillingLimitBlocksExceedingReservation(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-limit:")
	key := createBillingKey(t, s, 1000)

	if !reserve(t, s, key.ID, "event-a", 600, time.Hour) {
		t.Fatal("first reservation must succeed")
	}
	if reserve(t, s, key.ID, "event-b", 500, time.Hour) {
		t.Fatal("reservation exceeding the limit must be refused")
	}
	if !reserve(t, s, key.ID, "event-b", 400, time.Hour) {
		t.Fatal("reservation filling the limit exactly must succeed")
	}
	if reserve(t, s, key.ID, "event-c", 1, time.Hour) {
		t.Fatal("reservation above a full limit must be refused")
	}
	// Re-reserving the same event id with the same amount is idempotent.
	if !reserve(t, s, key.ID, "event-a", 600, time.Hour) {
		t.Fatal("re-reserving the same event must succeed")
	}
	// The same event id with a different amount is a conflict, not a silent
	// second hold.
	if _, err := s.ReserveApiKeyBilling(context.Background(), key.ID, "event-a", 700, time.Now().UTC().Add(time.Hour)); err == nil {
		t.Fatal("re-reserving with a different amount must fail")
	}
}

// TestApiKeyBillingExpiredReservationsStopCounting checks that a hold whose TTL
// passed frees its capacity on the next reservation attempt.
func TestApiKeyBillingExpiredReservationsStopCounting(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-expiry:")
	key := createBillingKey(t, s, 1000)

	if !reserve(t, s, key.ID, "event-expired", 900, -time.Minute) {
		t.Fatal("an already-expired hold is still recorded")
	}
	if !reserve(t, s, key.ID, "event-live", 900, time.Hour) {
		t.Fatal("expired capacity must be reclaimed")
	}
	if reserve(t, s, key.ID, "event-extra", 200, time.Hour) {
		t.Fatal("only the live hold may count")
	}
}

// TestApiKeyBillingSettleMovesReservationIntoUsed pins settlement: the hold
// disappears, the charge lands in the used counter, and actual usage is billed
// even when its hold is gone.
func TestApiKeyBillingSettleMovesReservationIntoUsed(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-settle:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	if !reserve(t, s, key.ID, "event-a", 400, time.Hour) {
		t.Fatal("reservation must succeed")
	}
	if err := s.SettleApiKeyBilling(ctx, key.ID, "event-a", 300); err != nil {
		t.Fatalf("SettleApiKeyBilling() error = %v", err)
	}
	// The hold is gone, so its capacity is neither free nor double counted.
	if !reserve(t, s, key.ID, "event-b", 700, time.Hour) {
		t.Fatal("used 300 + held 700 must fit the limit")
	}
	if reserve(t, s, key.ID, "event-c", 1, time.Hour) {
		t.Fatal("limit must now be full")
	}

	released, err := s.ReleaseApiKeyBilling(ctx, key.ID, "event-a")
	if err != nil {
		t.Fatalf("ReleaseApiKeyBilling() error = %v", err)
	}
	if released {
		t.Fatal("a settled reservation must not be released again")
	}

	// Settling an event whose hold already expired still charges: the request ran.
	if err := s.SettleApiKeyBilling(ctx, key.ID, "event-unknown", 100); err != nil {
		t.Fatalf("settling an unknown event error = %v", err)
	}
	got, err := s.GetApiKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID() error = %v", err)
	}
	if got.BillingUsedUSDTicks != 400 {
		t.Fatalf("used = %d, want 400", got.BillingUsedUSDTicks)
	}
	listed, err := s.ListApiKeys(ctx)
	if err != nil {
		t.Fatalf("ListApiKeys() error = %v", err)
	}
	if len(listed) != 1 || listed[0].BillingUsedUSDTicks != 400 || listed[0].BillingLimitUSDTicks != 1000 {
		t.Fatalf("ListApiKeys() = %#v", listed)
	}
}

// TestApiKeyBillingReleaseFreesCapacityAndReportsExistence covers the release
// path the request middleware relies on to avoid charging a failed request.
func TestApiKeyBillingReleaseFreesCapacityAndReportsExistence(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-release:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	if !reserve(t, s, key.ID, "event-a", 400, time.Hour) {
		t.Fatal("reservation must succeed")
	}
	released, err := s.ReleaseApiKeyBilling(ctx, key.ID, "event-a")
	if err != nil || !released {
		t.Fatalf("ReleaseApiKeyBilling() = %v, %v; want true, nil", released, err)
	}
	if !reserve(t, s, key.ID, "event-b", 1000, time.Hour) {
		t.Fatal("released capacity must be reusable")
	}
	released, err = s.ReleaseApiKeyBilling(ctx, key.ID, "event-a")
	if err != nil || released {
		t.Fatalf("second release = %v, %v; want false, nil", released, err)
	}
}

// TestApiKeyBillingResetZeroesUsedAndDropsReservations covers the admin reset.
func TestApiKeyBillingResetZeroesUsedAndDropsReservations(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-reset:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	if !reserve(t, s, key.ID, "event-a", 400, time.Hour) {
		t.Fatal("reservation must succeed")
	}
	if err := s.SettleApiKeyBilling(ctx, key.ID, "event-a", 400); err != nil {
		t.Fatalf("SettleApiKeyBilling() error = %v", err)
	}
	if err := s.ResetApiKeyBilling(ctx, key.ID); err != nil {
		t.Fatalf("ResetApiKeyBilling() error = %v", err)
	}
	got, err := s.GetApiKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID() error = %v", err)
	}
	if got.BillingUsedUSDTicks != 0 || got.BillingLimitUSDTicks != 1000 {
		t.Fatalf("after reset key = %#v", got)
	}
	if !reserve(t, s, key.ID, "event-b", 1000, time.Hour) {
		t.Fatal("a full limit must be available after a reset")
	}
}

// TestApiKeyBillingLimitMirrorFollowsKeyUpdates checks the Redis limit mirror is
// rewritten by UpdateApiKey, which is what the admin PATCH path uses.
func TestApiKeyBillingLimitMirrorFollowsKeyUpdates(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-mirror:")
	key := createBillingKey(t, s, 0)
	ctx := context.Background()

	if !reserve(t, s, key.ID, "event-unlimited", 10_000, time.Hour) {
		t.Fatal("a key without a limit must not be rationed")
	}
	if _, err := s.ReleaseApiKeyBilling(ctx, key.ID, "event-unlimited"); err != nil {
		t.Fatalf("ReleaseApiKeyBilling() error = %v", err)
	}

	key.BillingLimitUSDTicks = 100
	if err := s.UpdateApiKey(ctx, key); err != nil {
		t.Fatalf("UpdateApiKey() error = %v", err)
	}
	if reserve(t, s, key.ID, "event-limited", 200, time.Hour) {
		t.Fatal("the updated limit must be enforced immediately")
	}
	if !reserve(t, s, key.ID, "event-fits", 100, time.Hour) {
		t.Fatal("the updated limit must still admit a fitting request")
	}

	// Deleting the key drops its ledger keys with it.
	if err := s.DeleteApiKey(ctx, key.ID); err != nil {
		t.Fatalf("DeleteApiKey() error = %v", err)
	}
	if _, err := s.GetApiKeyByID(ctx, key.ID); !errors.Is(err, ErrNoRows) {
		t.Fatalf("GetApiKeyByID() after delete = %v, want ErrNoRows", err)
	}
}

// TestApiKeyBillingRejectsInvalidReservations pins the argument validation so a
// caller mistake cannot create an unbounded hold.
func TestApiKeyBillingRejectsInvalidReservations(t *testing.T) {
	s, _ := newApiKeyBillingStore(t, "billing-invalid:")
	ctx := context.Background()
	key := createBillingKey(t, s, 1000)

	if _, err := s.ReserveApiKeyBilling(ctx, key.ID, "", 10, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("an empty event id must be refused")
	}
	if _, err := s.ReserveApiKeyBilling(ctx, key.ID, "event", 0, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a zero amount must be refused")
	}
	if _, err := s.ReserveApiKeyBilling(ctx, key.ID, "event", 10, time.Time{}); err == nil {
		t.Fatal("a missing expiry must be refused")
	}
	if _, err := s.ReserveApiKeyBilling(ctx, 0, "event", 10, time.Now().Add(time.Hour)); !errors.Is(err, ErrNoRows) {
		t.Fatalf("reserving for key 0 = %v, want ErrNoRows", err)
	}
	if err := s.SettleApiKeyBilling(ctx, 0, "event", 10); !errors.Is(err, ErrNoRows) {
		t.Fatalf("settling key 0 = %v, want ErrNoRows", err)
	}
	if err := s.ResetApiKeyBilling(ctx, 0); !errors.Is(err, ErrNoRows) {
		t.Fatalf("resetting key 0 = %v, want ErrNoRows", err)
	}
}

// A key with a billing period starts a fresh period once it elapses, so a limit
// is per period rather than forever.
func TestApiKeyBillingPeriodRollsOver(t *testing.T) {
	s, mini := newApiKeyBillingStore(t, "period:")
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()

	now := time.Now().UTC()
	key := &ApiKey{
		Name: "period", KeyHash: "hash-period", Enabled: true,
		BillingLimitUSDTicks: 1_000_000, BillingPeriodDays: 1,
		BillingPeriodStartedAt: now.Add(-48 * time.Hour),
	}
	if err := s.CreateApiKey(ctx, key); err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if err := s.SettleApiKeyBilling(ctx, key.ID, "req_old", 500_000); err != nil {
		t.Fatalf("SettleApiKeyBilling: %v", err)
	}
	stored, err := s.GetApiKeyByID(ctx, key.ID)
	if err != nil || stored.BillingUsedUSDTicks != 500_000 {
		t.Fatalf("used=%d err=%v", stored.BillingUsedUSDTicks, err)
	}

	// Authorizing the key rolls an elapsed period over. The lookup hashes the raw
	// value, so the key is created with the hash of the raw value the caller uses.
	raw := "raw-period-key"
	digest := sha256.Sum256([]byte(raw))
	key.KeyHash = hex.EncodeToString(digest[:])
	if err := s.UpdateApiKey(ctx, key); err != nil {
		t.Fatalf("UpdateApiKey: %v", err)
	}
	if _, err := s.AuthorizeApiKey(ctx, raw); err != nil {
		t.Fatalf("AuthorizeApiKey: %v", err)
	}
	rolled, err := s.GetApiKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID: %v", err)
	}
	if rolled.BillingUsedUSDTicks != 0 {
		t.Fatalf("the period did not roll over: used=%d", rolled.BillingUsedUSDTicks)
	}
	if !rolled.BillingPeriodStartedAt.After(now.Add(-time.Minute)) {
		t.Fatalf("period start was not advanced: %v", rolled.BillingPeriodStartedAt)
	}
}

// Resetting billing by hand zeroes the counter without touching the limit.
func TestResetApiKeyBillingKeepsTheLimit(t *testing.T) {
	s, mini := newApiKeyBillingStore(t, "reset:")
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()

	key := &ApiKey{Name: "reset", KeyHash: "hash-reset", Enabled: true, BillingLimitUSDTicks: 2_000_000}
	if err := s.CreateApiKey(ctx, key); err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if err := s.SettleApiKeyBilling(ctx, key.ID, "req_1", 1_000_000); err != nil {
		t.Fatalf("SettleApiKeyBilling: %v", err)
	}
	if err := s.ResetApiKeyBilling(ctx, key.ID); err != nil {
		t.Fatalf("ResetApiKeyBilling: %v", err)
	}
	stored, err := s.GetApiKeyByID(ctx, key.ID)
	if err != nil {
		t.Fatalf("GetApiKeyByID: %v", err)
	}
	if stored.BillingUsedUSDTicks != 0 {
		t.Fatalf("used=%d want 0", stored.BillingUsedUSDTicks)
	}
	if stored.BillingLimitUSDTicks != 2_000_000 {
		t.Fatalf("the limit was lost: %d", stored.BillingLimitUSDTicks)
	}
	// Capacity is back: a reservation that the old usage would have blocked now
	// succeeds.
	ok, err := s.ReserveApiKeyBilling(ctx, key.ID, "req_2", 2_000_000, time.Now().Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("reserve after reset ok=%v err=%v", ok, err)
	}
}
