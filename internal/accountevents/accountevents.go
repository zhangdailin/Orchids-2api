// Package accountevents publishes account-change notifications inside one
// process.
//
// Before this existed, three caches answered "what does this account look like
// now?" on their own schedule: the account pool re-read Redis on a five-second
// TTL, the client cache compared fingerprints only when a request arrived, and
// the refresh scheduler read whatever snapshot the loop had loaded. A change
// therefore took effect at three different moments, and a reader could act on a
// credential that had just been replaced.
//
// The bus makes the write the trigger: a mutation that has been persisted
// publishes one change, and every subscriber reacts in the same instant.
//
// Boundary — this is a SINGLE-PROCESS bus. A second replica of this service does
// not receive these events; it still observes changes through its own cache TTL.
// Cross-instance delivery would need Redis pub/sub or a stream, and is not
// implemented here.
package accountevents

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/store"
)

// Kind describes what happened to an account.
type Kind string

const (
	KindCreated Kind = "created"
	// KindUpdated is a plain field change (name, weight, enabled, ...).
	KindUpdated Kind = "updated"
	// KindCredential means the credential or the provider routing changed, so a
	// cached client built from the old credential must not be reused.
	KindCredential Kind = "credential"
	// KindStatus covers health changes (status code, verdict stamp).
	KindStatus Kind = "status"
	KindDeleted Kind = "deleted"
)

// Change is one published account mutation.
type Change struct {
	AccountID int64
	Kind      Kind
	// Account is the state AFTER the write, or nil for a deletion.
	Account *store.Account
	// Previous is the state before the write, nil for a creation. It is what
	// lets a subscriber see which field actually moved instead of guessing.
	Previous *store.Account
	At       time.Time
}

// Subscriber receives coalesced change batches. Batch delivery is deliberate:
// a burst of writes to one account (a refresh loop touching a dozen fields) must
// not produce a dozen cache rebuilds.
type Subscriber interface {
	// AccountChanges is called with the account IDs that changed since the last
	// call. It must not block: the bus delivers on its own goroutine.
	AccountChanges(ids []int64)
}

// Kick is a subscriber that only signals that something changed, so a loop can
// wake early instead of waiting for its next tick. The channel is buffered and
// the signal is dropped when one is already pending, which is what keeps a burst
// of changes from queueing a burst of wake-ups.
type Kick struct {
	signal chan struct{}
}

// NewKick creates a Kick with a one-slot buffer.
func NewKick() *Kick { return &Kick{signal: make(chan struct{}, 1)} }

// AccountChanges implements Subscriber.
func (k *Kick) AccountChanges([]int64) {
	if k == nil {
		return
	}
	select {
	case k.signal <- struct{}{}:
	default:
	}
}

// Channel is the wake-up channel to select on.
func (k *Kick) Channel() <-chan struct{} {
	if k == nil {
		return nil
	}
	return k.signal
}

// Logger is the store's side of the contract. The store stays unaware of who
// listens; it only announces.
type Logger interface {
	Publish(change Change)
}

const (
	// publishBuffer bounds the queue between writers and the delivery loop, so a
	// stalled subscriber cannot slow down a write.
	publishBuffer = 1024
	// coalesceWindow is how long the bus waits before flushing a batch. It is
	// short enough that "a new request sees the change" is true in practice, and
	// long enough that a multi-field update collapses into one notification.
	coalesceWindow = 5 * time.Millisecond
)

// Bus is the in-process publisher.
type Bus struct {
	mu          sync.Mutex
	subscribers []Subscriber
	queue       chan Change
	started     bool
	stopOnce    sync.Once
	stop        chan struct{}
	// delivered counts coalesced batches, for tests and diagnostics.
	delivered int64
}

// NewBus creates an idle bus. Delivery starts with the first subscriber.
func NewBus() *Bus {
	return &Bus{
		queue: make(chan Change, publishBuffer),
		stop:  make(chan struct{}),
	}
}

// Publish queues a change. It never blocks and never fails the write that
// produced it: observability must not turn a successful write into a failed one.
func (b *Bus) Publish(change Change) {
	if b == nil {
		return
	}
	if change.At.IsZero() {
		change.At = time.Now()
	}
	select {
	case b.queue <- change:
	default:
		slog.Warn("Account change queue full; dropping notification", "account_id", change.AccountID, "kind", string(change.Kind))
	}
}

// Subscribe registers a subscriber and starts delivery.
func (b *Bus) Subscribe(subscriber Subscriber) {
	if b == nil || subscriber == nil {
		return
	}
	b.mu.Lock()
	b.subscribers = append(b.subscribers, subscriber)
	start := !b.started
	b.started = true
	b.mu.Unlock()
	if start {
		go b.deliverLoop()
	}
}

// Close stops delivery. It is used by tests and by shutdown.
func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() { close(b.stop) })
}

// Deliveries reports how many coalesced batches have been delivered.
func (b *Bus) Deliveries() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

// deliverLoop drains the queue, coalescing by account ID within a window before
// notifying every subscriber exactly once per batch.
func (b *Bus) deliverLoop() {
	timer := time.NewTimer(coalesceWindow)
	defer timer.Stop()
	pending := map[int64]struct{}{}

	flush := func() {
		if len(pending) == 0 {
			return
		}
		ids := make([]int64, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		pending = map[int64]struct{}{}

		b.mu.Lock()
		subscribers := append([]Subscriber(nil), b.subscribers...)
		b.delivered++
		b.mu.Unlock()

		for _, subscriber := range subscribers {
			notify(subscriber, ids)
		}
	}

	for {
		select {
		case <-b.stop:
			flush()
			return
		case change := <-b.queue:
			if change.AccountID != 0 {
				pending[change.AccountID] = struct{}{}
			}
			// Reset the window so a burst flushes as one batch.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(coalesceWindow)
		case <-timer.C:
			flush()
			timer.Reset(coalesceWindow)
		}
	}
}

// notify isolates a subscriber: one that panics or blocks must not stop delivery
// to the others, and a bad notification must never reach the write path.
func notify(subscriber Subscriber, ids []int64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Account change subscriber panicked", "error", r, "accounts", len(ids))
		}
	}()
	subscriber.AccountChanges(ids)
}

// Classify decides what kind of change a write represents. Credential changes
// take precedence: they are the ones that invalidate a cached client.
func Classify(previous, current *store.Account) Kind {
	switch {
	case previous == nil && current == nil:
		return KindUpdated
	case previous == nil:
		return KindCreated
	case current == nil:
		return KindDeleted
	}
	if credentialChanged(previous, current) {
		return KindCredential
	}
	if statusChanged(previous, current) {
		return KindStatus
	}
	return KindUpdated
}

// credentialMaterial is every field a client is built from. Comparing this set is
// what makes "the credential changed" precise instead of a guess.
type credentialMaterial struct {
	accountType  string
	sessionID    string
	clientCookie string
	refreshToken string
	deviceID     string
	requestID    string
	sessionCk    string
	clientUAT    string
	projectID    string
	token        string
	oauthAccess  string
	oauthRefresh string
	wbAccess     string
	wbRefresh    string
	wbUID        string
	agentMode    string
	grokProvider string
	credType     string
	upstreamMode string
	enabled      bool
	weight       int
}

func materialOf(acc *store.Account) credentialMaterial {
	if acc == nil {
		return credentialMaterial{}
	}
	return credentialMaterial{
		accountType:  strings.TrimSpace(acc.AccountType),
		sessionID:    acc.SessionID,
		clientCookie: acc.ClientCookie,
		refreshToken: acc.RefreshToken,
		deviceID:     acc.DeviceID,
		requestID:    acc.RequestID,
		sessionCk:    acc.SessionCookie,
		clientUAT:    acc.ClientUat,
		projectID:    acc.ProjectID,
		token:        acc.Token,
		oauthAccess:  acc.OAuthAccessToken,
		oauthRefresh: acc.OAuthRefreshToken,
		wbAccess:     acc.WorkBuddyAccessToken,
		wbRefresh:    acc.WorkBuddyRefreshToken,
		wbUID:        acc.WorkBuddyUID,
		agentMode:    acc.AgentMode,
		grokProvider: acc.GrokProvider,
		credType:     acc.CredentialType,
		upstreamMode: acc.UpstreamMode,
		enabled:      acc.Enabled,
		weight:       acc.Weight,
	}
}

func credentialChanged(previous, current *store.Account) bool {
	return materialOf(previous) != materialOf(current)
}

func statusChanged(previous, current *store.Account) bool {
	return previous.StatusCode != current.StatusCode ||
		previous.StatusMessage != current.StatusMessage ||
		!previous.VerifiedAt.Equal(current.VerifiedAt)
}
