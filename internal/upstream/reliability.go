package upstream

import (
	"time"

	"github.com/sony/gobreaker"
)

// CircuitBreaker wraps gobreaker with sensible defaults for API calls.
type CircuitBreaker struct {
	cb *gobreaker.CircuitBreaker
}

// CircuitBreakerConfig configures the circuit breaker.
type CircuitBreakerConfig struct {
	Name         string
	MaxRequests  uint32        // Requests allowed in half-open state
	Interval     time.Duration // Cyclic period for clearing counters
	Timeout      time.Duration // Time to wait before half-open
	FailureRatio float64       // Ratio of failures to trip
	MinRequests  uint32        // Min requests before evaluating ratio
}

// DefaultCircuitConfig returns sensible defaults.
func DefaultCircuitConfig(name string) CircuitBreakerConfig {
	return CircuitBreakerConfig{
		Name:         name,
		MaxRequests:  3,
		Interval:     60 * time.Second,
		Timeout:      30 * time.Second,
		FailureRatio: 0.5,
		MinRequests:  5,
	}
}

// NewCircuitBreaker creates a circuit breaker with the given config.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	settings := gobreaker.Settings{
		Name:        cfg.Name,
		MaxRequests: cfg.MaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < cfg.MinRequests {
				return false
			}
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return failureRatio >= cfg.FailureRatio
		},
	}
	return &CircuitBreaker{
		cb: gobreaker.NewCircuitBreaker(settings),
	}
}

// Execute runs the given function through the circuit breaker.
func (c *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
	return c.cb.Execute(fn)
}
