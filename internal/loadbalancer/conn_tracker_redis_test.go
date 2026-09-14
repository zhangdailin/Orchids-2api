package loadbalancer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisConnTrackerLeaseIsAtomicSharedAndReleased(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()

	first := NewRedisConnTracker(client, "test:")
	second := NewRedisConnTracker(client, "test:")
	if !first.TryAcquire(42, 1) {
		t.Fatal("first lease was rejected")
	}
	if second.TryAcquire(42, 1) {
		t.Fatal("second process exceeded the shared hard limit")
	}
	if got := second.GetCount(42); got != 1 {
		t.Fatalf("shared count=%d want 1", got)
	}
	// Constructing another tracker must never erase live leases from a peer.
	third := NewRedisConnTracker(client, "test:")
	if got := third.GetCount(42); got != 1 {
		t.Fatalf("new tracker cleared peer lease; count=%d", got)
	}
	first.Release(42)
	if got := second.GetCount(42); got != 0 {
		t.Fatalf("released lease count=%d want 0", got)
	}
}

func TestRedisConnTrackerReclaimsExpiredAndLegacyCounters(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "test:")
	key := tracker.key(7)

	if err := client.ZAdd(context.Background(), key, redis.Z{Score: float64(time.Now().Add(-time.Minute).UnixMilli()), Member: "dead"}).Err(); err != nil {
		t.Fatal(err)
	}
	if got := tracker.GetCount(7); got != 0 {
		t.Fatalf("expired lease count=%d want 0", got)
	}

	if err := client.Set(context.Background(), key, "99", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if !tracker.TryAcquire(7, 1) {
		t.Fatal("legacy string counter prevented lease migration")
	}
	if got := tracker.GetCount(7); got != 1 {
		t.Fatalf("migrated count=%d want 1", got)
	}
	tracker.Release(7)
}

func TestRedisConnTrackerCloseReleasesOwnedLeases(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()

	tracker := NewRedisConnTracker(client, "test:")
	peer := NewRedisConnTracker(client, "test:")
	if !tracker.TryAcquire(42, 2) || !tracker.TryAcquire(42, 2) {
		t.Fatal("failed to acquire owned leases")
	}
	tracker.Close()
	if got := peer.GetCount(42); got != 0 {
		t.Fatalf("leases remained after close: %d", got)
	}
	if tracker.TryAcquire(42, 1) {
		t.Fatal("closed tracker acquired a new lease")
	}
}
