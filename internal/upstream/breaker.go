package upstream

import (
	"sync"
)

// upstreamBreakers holds circuit breakers per account.
var upstreamBreakers = struct {
	sync.RWMutex
	breakers map[string]*CircuitBreaker
}{
	breakers: make(map[string]*CircuitBreaker),
}

// GetAccountBreaker returns or creates a circuit breaker for the given account.
func GetAccountBreaker(accountName string) *CircuitBreaker {
	upstreamBreakers.RLock()
	if cb, ok := upstreamBreakers.breakers[accountName]; ok {
		upstreamBreakers.RUnlock()
		return cb
	}
	upstreamBreakers.RUnlock()

	upstreamBreakers.Lock()
	defer upstreamBreakers.Unlock()

	// Double-check after acquiring write lock
	if cb, ok := upstreamBreakers.breakers[accountName]; ok {
		return cb
	}

	cfg := DefaultCircuitConfig("upstream-" + accountName)
	cb := NewCircuitBreaker(cfg)
	upstreamBreakers.breakers[accountName] = cb
	return cb
}

