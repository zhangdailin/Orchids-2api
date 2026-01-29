package middleware

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"
)

type ConcurrencyLimiter struct {
	sem         chan struct{}
	timeout     time.Duration
	activeCount int64
	totalReqs   int64
	rejectedReqs int64
}

func NewConcurrencyLimiter(maxConcurrent int, timeout time.Duration) *ConcurrencyLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 100
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &ConcurrencyLimiter{
		sem:     make(chan struct{}, maxConcurrent),
		timeout: timeout,
	}
}

func (cl *ConcurrencyLimiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cl.totalReqs, 1)
		
		ctx, cancel := context.WithTimeout(r.Context(), cl.timeout)
		defer cancel()

		select {
		case cl.sem <- struct{}{}:
			atomic.AddInt64(&cl.activeCount, 1)
			defer func() {
				<-cl.sem
				atomic.AddInt64(&cl.activeCount, -1)
			}()
			next.ServeHTTP(w, r.WithContext(ctx))
		case <-ctx.Done():
			atomic.AddInt64(&cl.rejectedReqs, 1)
			http.Error(w, "Request timeout or server busy", http.StatusServiceUnavailable)
		}
	}
}

func (cl *ConcurrencyLimiter) Stats() (active, total, rejected int64) {
	return atomic.LoadInt64(&cl.activeCount), 
		   atomic.LoadInt64(&cl.totalReqs), 
		   atomic.LoadInt64(&cl.rejectedReqs)
}
