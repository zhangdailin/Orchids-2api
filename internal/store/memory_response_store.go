package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// MemoryResponseTTL is the lifetime of an in-process stored response when the
// caller asks for no particular TTL. It mirrors the Redis default so switching
// backends never silently shortens a conversation.
const MemoryResponseTTL = 30 * 24 * time.Hour

// memoryResponseSweepThreshold bounds the map before an expired-entry sweep is
// attempted. Without it a long-lived process that stores many short-lived
// responses would only ever drop expired rows when they happen to be read again.
const memoryResponseSweepThreshold = 256

var (
	ErrMemoryResponseStoreNotConfigured = errors.New("memory response store not configured")
	ErrStoredResponseIdentityRequired   = errors.New("response id and owner are required")
)

// memoryStoredResponse is one entry of the in-process response store.
type memoryStoredResponse struct {
	record    StoredResponse
	expiresAt time.Time
}

// MemoryResponseStore is an in-process implementation of the stored-response
// contract, used when the gateway has no response backend to talk to.
//
// Redis stays the primary store: a stored response is resumed by whichever
// replica receives the next turn, so only a shared backend can serve a
// multi-replica deployment. This store exists for the single-process case — a
// gateway started without Redis, and tests that exercise the Responses bridge
// without standing one up — so store=true, previous_response_id and
// GET/DELETE /responses/{id} keep working instead of degrading to
// response_store_unavailable.
//
// Ownership is part of the key exactly as it is in Redis: a response id is
// readable only by the API-key fingerprint that created it. A miss and an
// expired row are both reported as ErrNoRows so callers keep the 404 they
// already produce for Redis.
type MemoryResponseStore struct {
	mu      sync.Mutex
	entries map[string]memoryStoredResponse
	ttl     time.Duration
	now     func() time.Time
}

// NewMemoryResponseStore returns a store whose records live for ttl. A
// non-positive ttl selects MemoryResponseTTL.
func NewMemoryResponseStore(ttl time.Duration) *MemoryResponseStore {
	if ttl <= 0 {
		ttl = MemoryResponseTTL
	}
	return &MemoryResponseStore{
		entries: make(map[string]memoryStoredResponse),
		ttl:     ttl,
		now:     time.Now,
	}
}

var defaultMemoryResponseStore = NewMemoryResponseStore(0)

// DefaultMemoryResponseStore returns the process-wide in-process response
// store. Callers that have a real store must keep using it: this one is the
// fallback for a gateway with no response backend, not a cache in front of one.
func DefaultMemoryResponseStore() *MemoryResponseStore { return defaultMemoryResponseStore }

func memoryResponseKey(responseID, ownerHash string) string {
	return ownerHash + "\x00" + responseID
}

func (s *MemoryResponseStore) timestamp() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// sweepLocked drops expired entries. The caller must hold s.mu.
func (s *MemoryResponseStore) sweepLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
}

// SaveStoredResponse implements the response-store contract.
func (s *MemoryResponseStore) SaveStoredResponse(_ context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil {
		return ErrMemoryResponseStoreNotConfigured
	}
	if response == nil {
		return ErrStoredResponseIdentityRequired
	}
	responseID := strings.TrimSpace(response.ResponseID)
	ownerHash := strings.TrimSpace(response.OwnerHash)
	if responseID == "" || ownerHash == "" {
		return ErrStoredResponseIdentityRequired
	}
	if ttl <= 0 {
		ttl = s.ttl
	}
	now := s.timestamp().UTC()
	stored := *response
	stored.ResponseID = responseID
	stored.OwnerHash = ownerHash
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	stored.UpdatedAt = now
	stored.ExpiresAt = now.Add(ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= memoryResponseSweepThreshold {
		s.sweepLocked(now)
	}
	s.entries[memoryResponseKey(responseID, ownerHash)] = memoryStoredResponse{record: stored, expiresAt: stored.ExpiresAt}
	return nil
}

// GetStoredResponse implements the response-store contract.
func (s *MemoryResponseStore) GetStoredResponse(_ context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil {
		return nil, ErrMemoryResponseStoreNotConfigured
	}
	responseID = strings.TrimSpace(responseID)
	ownerHash = strings.TrimSpace(ownerHash)
	if responseID == "" || ownerHash == "" {
		return nil, ErrNoRows
	}
	key := memoryResponseKey(responseID, ownerHash)

	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return nil, ErrNoRows
	}
	now := s.timestamp().UTC()
	if !entry.expiresAt.IsZero() && !now.Before(entry.expiresAt) {
		delete(s.entries, key)
		return nil, ErrNoRows
	}
	stored := entry.record
	return &stored, nil
}

// DeleteStoredResponse implements the response-store contract.
func (s *MemoryResponseStore) DeleteStoredResponse(_ context.Context, responseID, ownerHash string) error {
	if s == nil {
		return ErrMemoryResponseStoreNotConfigured
	}
	responseID = strings.TrimSpace(responseID)
	ownerHash = strings.TrimSpace(ownerHash)
	if responseID == "" || ownerHash == "" {
		return ErrNoRows
	}
	key := memoryResponseKey(responseID, ownerHash)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; !ok {
		return ErrNoRows
	}
	delete(s.entries, key)
	return nil
}
