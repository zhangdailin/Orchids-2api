// Package refreshqueue schedules credential refreshes by due time and collapses
// duplicate work per account.
//
// Before this existed the scheduler took "the next N accounts" from a global
// rotation offset, with no per-account lease. Two consequences followed: one
// account could be refreshed twice concurrently (its older snapshot winning the
// write-back and reinstating a status that had just been cleared), and an
// account that was due could sit behind an unrelated rotation for several
// cycles.
package refreshqueue

import (
	"sort"
	"sync"
	"time"
)

// Task is one account's pending refresh.
type Task struct {
	// AccountID is the key that deduplicates work.
	AccountID int64
	// Channel names the provider the refresh belongs to (grok, warp, ...). It is
	// informational: lease identity stays the account.
	Channel string
	// Due explains why the task is scheduled, for logs and tests.
	Due time.Duration
	// Stale reports whether a verdict is already needed (never verified) rather
	// than merely expired.
	Stale bool
	// Payload carries the caller's account object through the queue.
	Payload interface{}
}

// defaultHub is the process-wide refresh lease set. Every refresh entrance —
// the background scheduler, the admin "check" button, a per-channel refresh —
// takes a lease here, so "one refresh per account at a time" is a property of the
// process rather than of one loop.
var defaultHub = NewHub()

// Default returns the process-wide hub.
func Default() *Hub { return defaultHub }

// WithLease runs fn while holding the account's lease. It reports false without
// running fn when another refresh of the same account is already in flight,
// which is what stops an older snapshot from overwriting a newer one.
func WithLease(accountID int64, fn func()) bool {
	if !defaultHub.TryAcquire(accountID) {
		return false
	}
	defer defaultHub.Release(accountID)
	if fn != nil {
		fn()
	}
	return true
}

// Hub tracks which accounts are currently being refreshed and orders pending
// work by due time.
type Hub struct {
	mu       sync.Mutex
	inFlight map[int64]struct{}
	started  map[int64]time.Time
}

// NewHub creates an empty refresh hub.
func NewHub() *Hub {
	return &Hub{
		inFlight: map[int64]struct{}{},
		started:  map[int64]time.Time{},
	}
}

// TryAcquire takes the per-account lease. It reports false when another refresh
// of the same account is already running, which is the signal to merge the task
// instead of starting a second one.
func (h *Hub) TryAcquire(accountID int64) bool {
	if h == nil || accountID == 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, busy := h.inFlight[accountID]; busy {
		return false
	}
	h.inFlight[accountID] = struct{}{}
	h.started[accountID] = time.Now()
	return true
}

// Release frees the lease. Releasing an account that is not held is a no-op, so
// a deferred call cannot corrupt another task's lease.
func (h *Hub) Release(accountID int64) {
	if h == nil || accountID == 0 {
		return
	}
	h.mu.Lock()
	delete(h.inFlight, accountID)
	delete(h.started, accountID)
	h.mu.Unlock()
}

// InFlight reports whether the account currently holds a lease.
func (h *Hub) InFlight(accountID int64) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, busy := h.inFlight[accountID]
	return busy
}

// AcquiredAt reports when the current lease started, so a stuck refresh can be
// reported instead of silently blocking the account forever.
func (h *Hub) AcquiredAt(accountID int64) (time.Time, bool) {
	if h == nil {
		return time.Time{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	started, ok := h.started[accountID]
	return started, ok
}

// Len is the number of accounts currently being refreshed.
func (h *Hub) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inFlight)
}

// Plan orders tasks by due time — most overdue first — and drops the ones whose
// account already holds a lease or that exceed the concurrency limit. The
// returned slice is what the caller may start right now.
func Plan(tasks []Task, hub *Hub, maxConcurrent int) []Task {
	if len(tasks) == 0 {
		return nil
	}
	ordered := make([]Task, 0, len(tasks))
	seen := map[int64]struct{}{}
	for _, task := range tasks {
		if task.AccountID == 0 {
			continue
		}
		if _, duplicate := seen[task.AccountID]; duplicate {
			// The same account appearing twice in one plan is a caller bug, but
			// merging it here keeps the queue's promise: one task per account.
			continue
		}
		seen[task.AccountID] = struct{}{}
		ordered = append(ordered, task)
	}

	// Stable ordering by due time: the longest-overdue task is served first, and
	// equal due times keep the caller's order so tests stay deterministic.
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Due > ordered[j].Due
	})

	if maxConcurrent <= 0 {
		maxConcurrent = len(ordered)
	}
	out := make([]Task, 0, min(maxConcurrent, len(ordered)))
	for _, task := range ordered {
		if len(out) >= maxConcurrent {
			break
		}
		if hub != nil && hub.InFlight(task.AccountID) {
			continue
		}
		out = append(out, task)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
