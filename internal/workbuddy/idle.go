package workbuddy

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var errStreamIdleTimeout = errors.New("workbuddy stream idle timeout")

// idleMonitoringBody cancels a request only when its response body has been
// completely silent for the idle window. Every received byte renews the lease,
// so a long but active completion is unaffected.
type idleMonitoringBody struct {
	body     io.ReadCloser
	cancel   context.CancelFunc
	idle     time.Duration
	lastRead atomic.Int64
	timedOut atomic.Bool
	done     chan struct{}
	once     sync.Once
}

func monitorStreamIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if body == nil || idle <= 0 || cancel == nil {
		return body
	}
	monitored := &idleMonitoringBody{
		body:   body,
		cancel: cancel,
		idle:   idle,
		done:   make(chan struct{}),
	}
	monitored.lastRead.Store(time.Now().UnixNano())
	go monitored.watch()
	return monitored
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil && b.timedOut.Load() {
		return n, errStreamIdleTimeout
	}
	return n, err
}

func (b *idleMonitoringBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return b.body.Close()
}

func (b *idleMonitoringBody) watch() {
	ticker := time.NewTicker(idleCheckInterval(b.idle))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			last := time.Unix(0, b.lastRead.Load())
			if time.Since(last) >= b.idle {
				b.timedOut.Store(true)
				b.cancel()
				return
			}
		case <-b.done:
			return
		}
	}
}

func idleCheckInterval(idle time.Duration) time.Duration {
	interval := idle / 4
	if interval < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if interval > time.Second {
		return time.Second
	}
	return interval
}
