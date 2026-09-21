package puter

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"
)

func resetPuterRequestPacerForTest() {
	puterRequestPacer.Lock()
	clear(puterRequestPacer.entries)
	puterRequestPacer.Unlock()
}

func TestWaitForPuterRequestSlot_SkipsNonProductionHosts(t *testing.T) {
	resetPuterRequestPacerForTest()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	for range 2 {
		if err := waitForPuterRequestSlot(ctx, "http://127.0.0.1:8080", "token"); err != nil {
			t.Fatalf("non-production request was paced: %v", err)
		}
	}
}

func TestWaitForPuterRequestSlot_SharesOneTimerPerToken(t *testing.T) {
	resetPuterRequestPacerForTest()
	t.Cleanup(resetPuterRequestPacerForTest)
	if err := waitForPuterRequestSlot(context.Background(), defaultAPIURL, "shared-token"); err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	const waiters = 24
	contexts := make([]context.CancelFunc, 0, waiters)
	var wg sync.WaitGroup
	for range waiters {
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = waitForPuterRequestSlot(ctx, defaultAPIURL, "shared-token")
		}()
	}
	key := sha256.Sum256([]byte("shared-token"))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		puterRequestPacer.Lock()
		entry := puterRequestPacer.entries[key]
		ready := entry != nil && entry.waiters == waiters && entry.timerActive
		puterRequestPacer.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	puterRequestPacer.Lock()
	entry := puterRequestPacer.entries[key]
	if entry == nil || entry.waiters != waiters || !entry.timerActive {
		puterRequestPacer.Unlock()
		t.Fatalf("shared entry = %+v, want %d waiters and one active timer", entry, waiters)
	}
	puterRequestPacer.Unlock()
	for _, cancel := range contexts {
		cancel()
	}
	wg.Wait()
}

func TestWaitForPuterRequestSlot_HonorsContext(t *testing.T) {
	resetPuterRequestPacerForTest()
	t.Cleanup(resetPuterRequestPacerForTest)
	if err := waitForPuterRequestSlot(context.Background(), defaultAPIURL, "token"); err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	key := sha256.Sum256([]byte("token"))
	puterRequestPacer.Lock()
	before := puterRequestPacer.entries[key].next
	puterRequestPacer.Unlock()
	if err := waitForPuterRequestSlot(ctx, defaultAPIURL, "token"); err == nil {
		t.Fatal("expected queued request to honor context cancellation")
	}
	puterRequestPacer.Lock()
	after := puterRequestPacer.entries[key].next
	puterRequestPacer.Unlock()
	if !after.Equal(before) {
		t.Fatalf("canceled request reserved a future slot: before=%v after=%v", before, after)
	}
}
