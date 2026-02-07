package upstream

import (
	"sync"
	"time"
)

type breakerEntry struct {
	cb       *CircuitBreaker
	lastUsed time.Time
}

// upstreamBreakers holds circuit breakers per account.
var upstreamBreakers = struct {
	sync.RWMutex
	breakers   map[string]*breakerEntry
	lastCleanup time.Time
}{
	breakers: make(map[string]*breakerEntry),
}

const breakerMaxAge = 1 * time.Hour
const breakerCleanupInterval = 10 * time.Minute

// GetAccountBreaker returns or creates a circuit breaker for the given account.
func GetAccountBreaker(accountName string) *CircuitBreaker {
	upstreamBreakers.RLock()
	if entry, ok := upstreamBreakers.breakers[accountName]; ok {
		upstreamBreakers.RUnlock()
		// Update lastUsed under write lock
		upstreamBreakers.Lock()
		entry.lastUsed = time.Now()
		upstreamBreakers.Unlock()
		return entry.cb
	}
	upstreamBreakers.RUnlock()

	upstreamBreakers.Lock()
	defer upstreamBreakers.Unlock()

	// Double-check after acquiring write lock
	if entry, ok := upstreamBreakers.breakers[accountName]; ok {
		entry.lastUsed = time.Now()
		return entry.cb
	}

	cfg := DefaultCircuitConfig("upstream-" + accountName)
	cb := NewCircuitBreaker(cfg)
	upstreamBreakers.breakers[accountName] = &breakerEntry{
		cb:       cb,
		lastUsed: time.Now(),
	}
	cleanupBreakersLocked()
	return cb
}

// cleanupBreakersLocked removes stale breaker entries.
// Must be called with upstreamBreakers write lock held.
func cleanupBreakersLocked() {
	now := time.Now()
	if len(upstreamBreakers.breakers) < 128 && now.Sub(upstreamBreakers.lastCleanup) < breakerCleanupInterval {
		return
	}
	for name, entry := range upstreamBreakers.breakers {
		if now.Sub(entry.lastUsed) > breakerMaxAge {
			delete(upstreamBreakers.breakers, name)
		}
	}
	upstreamBreakers.lastCleanup = now
}
