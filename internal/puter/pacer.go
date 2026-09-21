package puter

import (
	"context"
	"crypto/sha256"
	"net/url"
	"strings"
	"sync"
	"time"
)

const puterRequestInterval = time.Second

type puterPacerEntry struct {
	next        time.Time
	wake        chan struct{}
	timerActive bool
	waiters     int
	lastUsed    time.Time
}

var puterRequestPacer = struct {
	sync.Mutex
	entries map[[32]byte]*puterPacerEntry
}{entries: make(map[[32]byte]*puterPacerEntry)}

// waitForPuterRequestSlot smooths bursts sent through the undocumented driver
// endpoint. The limit is per auth token, so independent Puter accounts can
// still make progress concurrently. Every token owns at most one timer: queued
// API calls wait on a shared wake channel instead of allocating N timers for the
// same deadline and waking the runtime's timer heap in a thundering herd.
func waitForPuterRequestSlot(ctx context.Context, rawURL, authToken string) error {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(endpoint.Hostname(), "api.puter.com") {
		return nil
	}

	key := sha256.Sum256([]byte(strings.TrimSpace(authToken)))
	for {
		now := time.Now()
		puterRequestPacer.Lock()
		entry := puterRequestPacer.entries[key]
		if entry == nil {
			entry = &puterPacerEntry{wake: make(chan struct{}), lastUsed: now}
			puterRequestPacer.entries[key] = entry
		}
		entry.lastUsed = now
		if !entry.next.After(now) {
			entry.next = now.Add(puterRequestInterval)
			cleanupPuterPacerLocked(now)
			puterRequestPacer.Unlock()
			return nil
		}

		wake := entry.wake
		entry.waiters++
		if !entry.timerActive {
			entry.timerActive = true
			deadline := entry.next
			time.AfterFunc(time.Until(deadline), func() { wakePuterPacerEntry(key, entry, deadline) })
		}
		puterRequestPacer.Unlock()

		select {
		case <-ctx.Done():
			puterRequestPacer.Lock()
			if current := puterRequestPacer.entries[key]; current == entry && entry.waiters > 0 {
				entry.waiters--
			}
			puterRequestPacer.Unlock()
			return ctx.Err()
		case <-wake:
			puterRequestPacer.Lock()
			if current := puterRequestPacer.entries[key]; current == entry && entry.waiters > 0 {
				entry.waiters--
			}
			puterRequestPacer.Unlock()
			// Compete for the now-open slot. Only the goroutine that advances next
			// returns; canceled waiters never reserve future capacity.
		}
	}
}

func wakePuterPacerEntry(key [32]byte, expected *puterPacerEntry, deadline time.Time) {
	puterRequestPacer.Lock()
	defer puterRequestPacer.Unlock()
	entry := puterRequestPacer.entries[key]
	if entry != expected || !entry.timerActive || !entry.next.Equal(deadline) {
		return
	}
	entry.timerActive = false
	close(entry.wake)
	entry.wake = make(chan struct{})
}

func cleanupPuterPacerLocked(now time.Time) {
	if len(puterRequestPacer.entries) <= 256 {
		return
	}
	cutoff := now.Add(-time.Minute)
	for key, entry := range puterRequestPacer.entries {
		if entry.waiters == 0 && !entry.timerActive && entry.lastUsed.Before(cutoff) {
			delete(puterRequestPacer.entries, key)
		}
	}
}
