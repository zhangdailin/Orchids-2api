package util

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// MonitorReadIdle interrupts a response body that produces no bytes for the
// configured window. Active long-running streams are unaffected.
func MonitorReadIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc, label string) io.ReadCloser {
	if body == nil || idle <= 0 || cancel == nil {
		return body
	}
	if label == "" {
		label = "upstream"
	}
	monitored := &readIdleBody{
		body: body, cancel: cancel, idle: idle,
		timeoutErr: errors.New(label + " stream idle timeout"), done: make(chan struct{}),
	}
	monitored.lastRead.Store(time.Now().UnixNano())
	go monitored.watch()
	return monitored
}

type readIdleBody struct {
	body       io.ReadCloser
	cancel     context.CancelFunc
	idle       time.Duration
	timeoutErr error
	lastRead   atomic.Int64
	timedOut   atomic.Bool
	done       chan struct{}
	once       sync.Once
}

func (b *readIdleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil && b.timedOut.Load() {
		return n, b.timeoutErr
	}
	return n, err
}

func (b *readIdleBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return b.body.Close()
}

func (b *readIdleBody) watch() {
	interval := b.idle / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if time.Since(time.Unix(0, b.lastRead.Load())) >= b.idle {
				b.timedOut.Store(true)
				b.cancel()
				return
			}
		case <-b.done:
			return
		}
	}
}
