package puter

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func resetPuterRequestPacerForTest() {
	puterRequestPacer.Lock()
	clear(puterRequestPacer.next)
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
	before := puterRequestPacer.next[key]
	puterRequestPacer.Unlock()
	if err := waitForPuterRequestSlot(ctx, defaultAPIURL, "token"); err == nil {
		t.Fatal("expected queued request to honor context cancellation")
	}
	puterRequestPacer.Lock()
	after := puterRequestPacer.next[key]
	puterRequestPacer.Unlock()
	if !after.Equal(before) {
		t.Fatalf("canceled request reserved a future slot: before=%v after=%v", before, after)
	}
}
