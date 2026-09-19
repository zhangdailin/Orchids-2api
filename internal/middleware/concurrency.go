package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// ConcurrencyLimiter limits concurrent request processing using a weighted semaphore.
// This is more efficient than channel-based semaphore for high-throughput scenarios.
type ConcurrencyLimiter struct {
	// 64-bit atomic fields must be at the top for 32-bit alignment
	activeCount  int64
	rejectedReqs int64
	cachedP95    int64 // Cached P95 to avoid sorting on the hot path

	sem     *semaphore.Weighted
	timeout time.Duration

	// Adaptive timeout
	adaptive      bool
	latencyWindow []int64 // Milliseconds
	windowIdx     int
	mu            sync.RWMutex

	lastP95Update time.Time
}

// NewConcurrencyLimiter creates a new limiter with the specified max concurrent requests and timeout.
func NewConcurrencyLimiter(maxConcurrent int, timeout time.Duration, adaptive bool) *ConcurrencyLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 100
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &ConcurrencyLimiter{
		sem:           semaphore.NewWeighted(int64(maxConcurrent)),
		timeout:       timeout,
		adaptive:      adaptive,
		latencyWindow: make([]int64, 100), // Keep last 100 requests
	}
}

func (cl *ConcurrencyLimiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return cl.limit(next, true)
}

// LimitLongLived applies the same admission and concurrency accounting as
// Limit but does not impose the request execution timeout. It is intended for
// authenticated WebSocket sessions whose lifetime is controlled by either
// peer, the server shutdown context, or upstream network deadlines.
func (cl *ConcurrencyLimiter) LimitLongLived(next http.HandlerFunc) http.HandlerFunc {
	return cl.limit(next, false)
}

func (cl *ConcurrencyLimiter) limit(next http.HandlerFunc, executionTimeout bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Admission is deliberately non-blocking. Queueing requests behind the
		// semaphore consumes connections and goroutines precisely when the server
		// is overloaded, which can amplify an overload into a broader outage.
		if !cl.sem.TryAcquire(1) {
			atomic.AddInt64(&cl.rejectedReqs, 1)
			slog.Warn("Concurrency limit: Request rejected", "total_rejected", atomic.LoadInt64(&cl.rejectedReqs))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"server is overloaded; retry later","type":"server_error","code":"server_overloaded","param":null}}`))
			return
		}

		slog.Debug("Concurrency limit: Slot acquired", "active", atomic.LoadInt64(&cl.activeCount)+1)

		atomic.AddInt64(&cl.activeCount, 1)
		reqStart := time.Now()

		defer func() {
			cl.sem.Release(1)
			atomic.AddInt64(&cl.activeCount, -1)

			duration := time.Since(reqStart)
			if cl.adaptive {
				cl.UpdateStats(duration)
			}
			slog.Debug("Concurrency limit: Slot released", "active", atomic.LoadInt64(&cl.activeCount), "duration", duration)
		}()

		if !executionTimeout {
			slog.Debug("Concurrency limit: Serving long-lived request", "path", r.URL.Path)
			next.ServeHTTP(w, r)
			return
		}

		// Use the full concurrency timeout for ordinary request execution.
		execCtx, cancelExec := context.WithTimeout(r.Context(), cl.timeout)
		defer cancelExec()
		slog.Debug("Concurrency limit: Serving request", "path", r.URL.Path, "timeout", cl.timeout)
		next.ServeHTTP(w, r.WithContext(execCtx))
	}
}

// UpdateStats records request latency for adaptive timeout
func (cl *ConcurrencyLimiter) UpdateStats(d time.Duration) {
	ms := d.Milliseconds()
	cl.mu.Lock()
	cl.latencyWindow[cl.windowIdx] = ms
	cl.windowIdx = (cl.windowIdx + 1) % len(cl.latencyWindow)

	// Update cached P95 periodically (e.g. at most once per second or every 10 requests) to avoid hot path bottleneck
	now := time.Now()
	shouldRecalc := now.Sub(cl.lastP95Update) > time.Second
	cl.mu.Unlock()

	if shouldRecalc {
		cl.recalcP95()
	}
}

// recalcP95 recalculates and caches the 95th percentile latency
func (cl *ConcurrencyLimiter) recalcP95() {
	cl.mu.Lock()
	now := time.Now()

	// Double-checked locking to avoid concurrent recalculations
	if now.Sub(cl.lastP95Update) <= time.Second {
		cl.mu.Unlock()
		return
	}
	cl.lastP95Update = now

	// Make a copy of the window to avoid holding the lock while sorting
	localWindow := make([]int64, len(cl.latencyWindow))
	copy(localWindow, cl.latencyWindow)
	cl.mu.Unlock()

	// Filter out zeros (uninitialized slots) to avoid skewing the result
	valid := make([]int64, 0, len(localWindow))
	for _, v := range localWindow {
		if v > 0 {
			valid = append(valid, v)
		}
	}
	if len(valid) < 10 {
		return // Not enough data
	}

	slices.Sort(valid)
	idx := int(float64(len(valid)) * 0.95)

	atomic.StoreInt64(&cl.cachedP95, valid[idx])
}
