package grok

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// tokenBucket is a simple thread-safe token bucket rate limiter.
type tokenBucket struct {
	rate       float64   // tokens per second
	burst      float64   // max tokens (burst capacity)
	tokens     float64   // current tokens
	lastUpdate time.Time // last token refill
	mu         sync.Mutex
}

func newTokenBucket(ratePerSec, burst float64) *tokenBucket {
	return &tokenBucket{
		rate:       ratePerSec,
		burst:      burst,
		tokens:     burst, // start full
		lastUpdate: time.Now(),
	}
}

// wait blocks until a token is available or ctx is cancelled.
// Returns nil if a token was acquired, or ctx.Err() if cancelled.
func (tb *tokenBucket) wait(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(tb.lastUpdate).Seconds()
		tb.tokens += elapsed * tb.rate
		if tb.tokens > tb.burst {
			tb.tokens = tb.burst
		}
		tb.lastUpdate = now

		if tb.tokens >= 1.0 {
			tb.tokens -= 1.0
			tb.mu.Unlock()
			return nil
		}
		// Calculate wait time for next token
		waitTime := time.Duration((1.0 - tb.tokens) / tb.rate * float64(time.Second))
		tb.mu.Unlock()

		if waitTime < 50*time.Millisecond {
			waitTime = 50 * time.Millisecond
		}
		if waitTime > 5*time.Second {
			waitTime = 5 * time.Second
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
		}
	}
}

// endpointRateLimiters holds per-endpoint token buckets.
var (
	endpointRateLimiters  = map[string]*tokenBucket{}
	endpointRateLimiterMu sync.Mutex
)

func init() {
	// App Chat API: 5 RPS with burst 10 (generous, adjust as needed).
	endpointRateLimiters["grok.com"] = newTokenBucket(5, 10)
	// Rate-limits check: 0.5 RPS with burst 1 (very conservative).
	endpointRateLimiters["grok.com/rate-limits"] = newTokenBucket(0.5, 1)
}

// rateLimitEndpoint blocks until it is safe to call the given endpoint URL.
// The endpoint is identified by a key: "grok.com" or "grok.com/rate-limits".
func rateLimitEndpoint(ctx context.Context, endpointKey string) error {
	endpointRateLimiterMu.Lock()
	tb := endpointRateLimiters[endpointKey]
	endpointRateLimiterMu.Unlock()
	if tb == nil {
		return nil
	}
	if err := tb.wait(ctx); err != nil {
		return err
	}
	slog.Debug("Rate limiter: token acquired", "endpoint", endpointKey)
	return nil
}

// appChatRateLimitEndpoint waits for the grok.com App Chat rate limiter.
func appChatRateLimitEndpoint(ctx context.Context) error {
	return rateLimitEndpoint(ctx, "grok.com")
}

// rateLimitsCheckEndpoint waits for the grok.com rate-limits rate limiter.
func rateLimitsCheckEndpoint(ctx context.Context) error {
	return rateLimitEndpoint(ctx, "grok.com/rate-limits")
}
