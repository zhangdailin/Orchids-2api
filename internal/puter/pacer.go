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

var puterRequestPacer = struct {
	sync.Mutex
	next map[[32]byte]time.Time
}{next: make(map[[32]byte]time.Time)}

// waitForPuterRequestSlot smooths bursts sent through the undocumented driver
// endpoint. The limit is per auth token, so independent Puter accounts can
// still make progress concurrently.
func waitForPuterRequestSlot(ctx context.Context, rawURL, authToken string) error {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(endpoint.Hostname(), "api.puter.com") {
		return nil
	}

	key := sha256.Sum256([]byte(strings.TrimSpace(authToken)))
	for {
		now := time.Now()
		puterRequestPacer.Lock()
		next := puterRequestPacer.next[key]
		if !next.After(now) {
			puterRequestPacer.next[key] = now.Add(puterRequestInterval)
			if len(puterRequestPacer.next) > 256 {
				cutoff := now.Add(-time.Minute)
				for candidate, candidateNext := range puterRequestPacer.next {
					if candidateNext.Before(cutoff) {
						delete(puterRequestPacer.next, candidate)
					}
				}
			}
			puterRequestPacer.Unlock()
			return nil
		}
		puterRequestPacer.Unlock()

		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
			// Compete for the now-open slot. Only the goroutine that advances
			// next returns; canceled waiters never reserve future capacity.
		}
	}
}
