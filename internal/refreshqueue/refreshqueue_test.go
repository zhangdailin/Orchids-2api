package refreshqueue

import (
	"sync"
	"testing"
	"time"
)

// TestHub_CollapsesDuplicateWork is the queue's core promise: a second refresh
// of the same account must be merged, not started while the first is running.
func TestHub_CollapsesDuplicateWork(t *testing.T) {
	hub := NewHub()
	if !hub.TryAcquire(7) {
		t.Fatal("first lease must be granted")
	}
	if hub.TryAcquire(7) {
		t.Fatal("a second concurrent lease for the same account must be refused")
	}
	if hub.Len() != 1 {
		t.Fatalf("in-flight = %d, want 1", hub.Len())
	}
	hub.Release(7)
	if !hub.TryAcquire(7) {
		t.Fatal("the lease must be reusable after release")
	}
	if _, ok := hub.AcquiredAt(7); !ok {
		t.Fatal("a held lease must report when it started")
	}
}

// TestHub_ReleaseIsIdempotent keeps a deferred release from stealing another
// task's lease.
func TestHub_ReleaseIsIdempotent(t *testing.T) {
	hub := NewHub()
	hub.TryAcquire(1)
	hub.Release(1)
	hub.Release(1)
	if hub.InFlight(1) {
		t.Fatal("released account still reports in-flight")
	}
	if !hub.TryAcquire(1) {
		t.Fatal("lease must be available after a double release")
	}
}

// TestPlan_OrdersByDueTime pins "most overdue first" so an account that missed
// several cycles is served before one that just expired.
func TestPlan_OrdersByDueTime(t *testing.T) {
	tasks := []Task{
		{AccountID: 1, Due: time.Minute},
		{AccountID: 2, Due: 2 * time.Hour},
		{AccountID: 3, Due: 10 * time.Minute},
	}
	planned := Plan(tasks, NewHub(), 0)
	got := []int64{planned[0].AccountID, planned[1].AccountID, planned[2].AccountID}
	want := []int64{2, 3, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("plan order = %v, want %v", got, want)
		}
	}
}

// TestPlan_SkipsBusyAccounts is the write-back guard's other half: an account
// already being refreshed is never scheduled twice, so an older snapshot cannot
// race a newer one.
func TestPlan_SkipsBusyAccounts(t *testing.T) {
	hub := NewHub()
	hub.TryAcquire(2)
	defer hub.Release(2)

	planned := Plan([]Task{
		{AccountID: 1, Due: time.Minute},
		{AccountID: 2, Due: time.Hour},
	}, hub, 0)

	if len(planned) != 1 || planned[0].AccountID != 1 {
		t.Fatalf("planned = %+v, want only the free account", planned)
	}
}

// TestPlan_RespectsConcurrencyLimit keeps a refresh burst bounded.
func TestPlan_RespectsConcurrencyLimit(t *testing.T) {
	planned := Plan([]Task{
		{AccountID: 1, Due: time.Minute},
		{AccountID: 2, Due: 2 * time.Minute},
		{AccountID: 3, Due: 3 * time.Minute},
	}, NewHub(), 2)
	if len(planned) != 2 {
		t.Fatalf("planned = %d, want 2", len(planned))
	}
	if planned[0].AccountID != 3 || planned[1].AccountID != 2 {
		t.Fatalf("limit must take the most overdue tasks, got %+v", planned)
	}
}

// TestPlan_MergesDuplicatesWithinOnePlan covers a caller that lists an account
// twice (for example source plus linked view).
func TestPlan_MergesDuplicatesWithinOnePlan(t *testing.T) {
	planned := Plan([]Task{
		{AccountID: 5, Due: time.Minute},
		{AccountID: 5, Due: time.Hour},
	}, NewHub(), 0)
	if len(planned) != 1 {
		t.Fatalf("planned = %d, want one merged task", len(planned))
	}
}

// TestPlan_EmptyInput is the idle path.
func TestPlan_EmptyInput(t *testing.T) {
	if planned := Plan(nil, NewHub(), 5); planned != nil {
		t.Fatalf("planned = %+v, want nil", planned)
	}
}

// TestHub_ConcurrentAcquire grants exactly one lease under contention.
func TestHub_ConcurrentAcquire(t *testing.T) {
	hub := NewHub()
	var wg sync.WaitGroup
	granted := make(chan struct{}, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if hub.TryAcquire(99) {
				granted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(granted)
	count := 0
	for range granted {
		count++
	}
	if count != 1 {
		t.Fatalf("leases granted = %d, want exactly 1", count)
	}
}

// TestWithLease_MergesConcurrentRefreshes pins the process-wide lease shared by
// the scheduler and the manual "check" path: the second caller must not run, so
// an older snapshot cannot be written over a newer verdict.
func TestWithLease_MergesConcurrentRefreshes(t *testing.T) {
	ran := make(chan struct{}, 4)
	started := make(chan struct{})
	release := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		WithLease(4242, func() {
			ran <- struct{}{}
			close(started)
			<-release
		})
	}()
	<-started

	if WithLease(4242, func() { ran <- struct{}{} }) {
		t.Fatal("a second lease for the same account must be refused")
	}
	close(release)
	<-done

	if len(ran) != 1 {
		t.Fatalf("lease body ran %d times, want exactly 1", len(ran))
	}
	// After the first lease is released the account is refreshable again.
	if !WithLease(4242, func() { ran <- struct{}{} }) {
		t.Fatal("the lease must be reusable once released")
	}
	if len(ran) != 2 {
		t.Fatalf("lease body ran %d times, want 2 after release", len(ran))
	}
}

// TestDefault_IsSharedAcrossCallers guards the wiring: the scheduler and the API
// must observe the same set, which is only true if Default() is a singleton.
func TestDefault_IsSharedAcrossCallers(t *testing.T) {
	if Default() != Default() {
		t.Fatal("Default() must return one process-wide hub")
	}
	Default().TryAcquire(99)
	t.Cleanup(func() { Default().Release(99) })
	if !Default().InFlight(99) {
		t.Fatal("a lease taken on Default() must be visible to every caller")
	}
}