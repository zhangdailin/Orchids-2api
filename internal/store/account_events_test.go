package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// recordingEmitter captures the notifications the store publishes.
type recordingEmitter struct {
	mu      sync.Mutex
	changes []AccountChange
}

func (e *recordingEmitter) Publish(change AccountChange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changes = append(e.changes, change)
}

// waitForChanges lets the asynchronous emitter settle before assertions.
func (e *recordingEmitter) waitForChanges(t *testing.T, want int) []AccountChange {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if changes := e.all(); len(changes) >= want {
			return changes
		}
		time.Sleep(2 * time.Millisecond)
	}
	return e.all()
}

func (e *recordingEmitter) all() []AccountChange {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AccountChange(nil), e.changes...)
}

func newEmitterStore(t *testing.T) (*Store, *recordingEmitter) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "events:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	emitter := &recordingEmitter{}
	s.SetChangeEmitter(emitter)
	return s, emitter
}

// TestChangeEmitter_PublishesOnlyAfterAPersistedWrite pins the contract the whole
// notification chain rests on: no event for a write that did not happen.
func TestChangeEmitter_PublishesOnlyAfterAPersistedWrite(t *testing.T) {
	s, emitter := newEmitterStore(t)
	ctx := context.Background()

	acc := &Account{AccountType: "warp", RefreshToken: "session-a", Enabled: true, Weight: 1}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	changes := emitter.waitForChanges(t, 1)
	if len(changes) != 1 {
		t.Fatalf("create published %d changes, want 1", len(changes))
	}
	if changes[0].AccountID != acc.ID || changes[0].Previous != nil {
		t.Fatalf("create change = %+v", changes[0])
	}

	// An update carries the previous state, which is what lets a subscriber see
	// whether the credential moved.
	updated, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	updated.RefreshToken = "session-b"
	if err := s.UpdateAccount(ctx, updated); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	changes = emitter.waitForChanges(t, 2)
	if len(changes) != 2 {
		t.Fatalf("update published %d changes, want 2 total", len(changes))
	}
	if changes[1].Previous == nil || changes[1].Previous.RefreshToken != "session-a" {
		t.Fatalf("update change lost the previous state: %+v", changes[1])
	}
	// The store publishes the id and the previous state; the after-state is the
	// emitter's to resolve, so it is not asserted here.
	if changes[1].AccountID != acc.ID {
		t.Fatalf("update change names the wrong account: %+v", changes[1])
	}

	// Deleting publishes once, and deleting a missing id publishes nothing.
	if err := s.DeleteAccount(ctx, acc.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if got := len(emitter.waitForChanges(t, 3)); got != 3 {
		t.Fatalf("delete published %d changes, want 3 total", got)
	}
	if err := s.DeleteAccount(ctx, acc.ID); err != nil {
		t.Fatalf("second DeleteAccount: %v", err)
	}
	if got := len(emitter.waitForChanges(t, 3)); got != 3 {
		t.Fatalf("deleting a missing account published an event (total %d)", got)
	}
}

// TestChangeEmitter_SilentWhenNoEmitterConfigured keeps a plain store (tests, a
// deployment without the bus) working.
func TestChangeEmitter_SilentWhenNoEmitterConfigured(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "silent:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	acc := &Account{AccountType: "warp", RefreshToken: "x", Enabled: true}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount with no emitter: %v", err)
	}
	if err := s.DeleteAccount(context.Background(), acc.ID); err != nil {
		t.Fatalf("DeleteAccount with no emitter: %v", err)
	}
}

// TestChangeEmitter_DoesNotBlockTheWrite keeps observability out of the write
// path: a slow emitter must not turn a successful write into a failed one.
func TestChangeEmitter_DoesNotBlockTheWrite(t *testing.T) {
	s, _ := newEmitterStore(t)
	release := make(chan struct{})
	s.SetChangeEmitter(blockingEmitter{release: release})
	t.Cleanup(func() { close(release) })

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- s.CreateAccount(context.Background(), &Account{AccountType: "puter", Token: "t", Enabled: true})
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked emitter stalled the write")
	}

	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) == 0 {
		t.Fatalf("the write did not land: %v", err)
	}
}

type blockingEmitter struct{ release chan struct{} }

func (b blockingEmitter) Publish(AccountChange) { <-b.release }

// TestChangeEmitter_IgnoresWritesToAMissingRow documents the delete-then-write
// race: the store's UpdateAccount is a documented no-op for a row that is gone,
// so it must not announce a change either. A subscriber never rebuilds a cache
// entry for an account that does not exist.
func TestChangeEmitter_IgnoresWritesToAMissingRow(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "missing:"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	emitter := &recordingEmitter{}
	s.SetChangeEmitter(emitter)

	acc := &Account{AccountType: "warp", RefreshToken: "session", Enabled: true}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.DeleteAccount(context.Background(), acc.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	afterDelete := len(emitter.waitForChanges(t, 2))
	if afterDelete != 2 {
		t.Fatalf("changes after create+delete = %d, want 2", afterDelete)
	}

	// The row is gone: the update is a no-op and must stay silent.
	acc.Weight = 5
	if err := s.UpdateAccount(context.Background(), acc); err != nil {
		t.Fatalf("UpdateAccount on a missing row: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(emitter.all()); got != afterDelete {
		t.Fatalf("a write to a missing row published an event (total %d, want %d)", got, afterDelete)
	}
}
