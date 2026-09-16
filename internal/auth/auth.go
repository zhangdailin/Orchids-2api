package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	sessionTokenLength = 32
	sessionTTL         = 7 * 24 * time.Hour
)

// SessionBackend persists admin sessions outside this process. The admin UI
// polls the gateway continuously, so a session that exists only in memory makes
// every restart — including every deploy — look like the site is broken: the
// browser keeps presenting a cookie the fresh process has never seen, the
// polling endpoints answer 401 and the UI bounces back to the login page.
type SessionBackend interface {
	SaveSession(ctx context.Context, token string, expiry time.Time) error
	HasSession(ctx context.Context, token string) (bool, error)
	DeleteSession(ctx context.Context, token string)
}

var (
	sessionBackendMu sync.RWMutex
	sessionBackend   SessionBackend
)

// SetSessionBackend installs the durable session store. Passing nil keeps the
// in-process map, which is also the fallback whenever the backend cannot be
// reached, so a persistence outage degrades to the previous behaviour instead
// of locking the operator out.
func SetSessionBackend(backend SessionBackend) {
	sessionBackendMu.Lock()
	sessionBackend = backend
	sessionBackendMu.Unlock()
}

func durableSessionBackend() SessionBackend {
	sessionBackendMu.RLock()
	defer sessionBackendMu.RUnlock()
	return sessionBackend
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
	// localOnly holds sessions minted while the durable write failed. They stay
	// valid for this process so a transient persistence error cannot break the
	// login that just succeeded, and they are deliberately not trusted after a
	// restart.
	localOnly map[string]time.Time
	// backendOnly holds sessions this process never issued but the durable store
	// confirmed. They are the process's proof that a restart recovered a live
	// operator cookie, so a later backend read failure can still accept it
	// instead of signing the operator out mid-session.
	backendOnly map[string]time.Time
}

// globalSessionStore mirrors every session this process issued. It is the
// authoritative store when no durable backend is configured.
var globalSessionStore = &SessionStore{
	sessions:    make(map[string]time.Time),
	localOnly:   make(map[string]time.Time),
	backendOnly: make(map[string]time.Time),
}

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanupExpiredSessions()
		}
	}()
}

func GenerateSessionToken() (string, error) {
	bytes := make([]byte, sessionTokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	token := hex.EncodeToString(bytes)
	expiry := time.Now().Add(sessionTTL)

	if backend := durableSessionBackend(); backend != nil {
		if err := backend.SaveSession(context.Background(), token, expiry); err != nil {
			slog.Warn("Failed to persist admin session; keeping it for this process only", "error", err)
			rememberSession(token, expiry)
			rememberLocalOnlySession(token, expiry)
			return token, nil
		}
	}
	rememberSession(token, expiry)

	return token, nil
}

func ValidateSessionToken(token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	if localOnlyHasSession(token) {
		return true
	}
	if backend := durableSessionBackend(); backend != nil {
		valid, err := backend.HasSession(context.Background(), token)
		if err == nil {
			if valid {
				// Remember that this process positively confirmed the session.
				// Without it, the next backend blip would reject a cookie the
				// backend itself handed back after the restart.
				rememberBackendSession(token)
				return true
			}
			// The backend is reachable and does not know this token. That is an
			// authoritative negative — it is what makes logout, revocation and an
			// expired TTL take effect — so it is not a reason to fall back.
			forgetSession(token)
			return false
		}
		// A backend outage must not sign out a session this process can still
		// vouch for: it either issued the token itself, or the backend confirmed
		// it earlier in this process's life. A token with neither record still
		// fails closed, so an outage never becomes an open door.
		slog.Warn("Admin session backend unavailable; using the in-process store", "error", err)
		return backendHasSession(token) || memoryHasSession(token)
	}
	return memoryHasSession(token)
}

func InvalidateSessionToken(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	forgetSession(token)
	if backend := durableSessionBackend(); backend != nil {
		backend.DeleteSession(context.Background(), token)
	}
}

func rememberSession(token string, expiry time.Time) {
	globalSessionStore.mu.Lock()
	globalSessionStore.sessions[token] = expiry
	globalSessionStore.mu.Unlock()
}

func rememberLocalOnlySession(token string, expiry time.Time) {
	globalSessionStore.mu.Lock()
	globalSessionStore.localOnly[token] = expiry
	globalSessionStore.mu.Unlock()
}

// rememberBackendSession records a session the durable store confirmed. Its
// lifetime is bounded by the session TTL because the backend does not report the
// remaining time; the next successful backend read extends the record.
func rememberBackendSession(token string) {
	globalSessionStore.mu.Lock()
	globalSessionStore.backendOnly[token] = time.Now().Add(sessionTTL)
	globalSessionStore.mu.Unlock()
}

func backendHasSession(token string) bool {
	globalSessionStore.mu.RLock()
	expiry, exists := globalSessionStore.backendOnly[token]
	globalSessionStore.mu.RUnlock()

	if !exists {
		return false
	}
	if time.Now().After(expiry) {
		forgetSession(token)
		return false
	}
	return true
}

func memoryHasSession(token string) bool {
	globalSessionStore.mu.RLock()
	expiry, exists := globalSessionStore.sessions[token]
	globalSessionStore.mu.RUnlock()

	if !exists {
		return false
	}

	if time.Now().After(expiry) {
		forgetSession(token)
		return false
	}

	return true
}

func localOnlyHasSession(token string) bool {
	globalSessionStore.mu.RLock()
	expiry, exists := globalSessionStore.localOnly[token]
	globalSessionStore.mu.RUnlock()

	if !exists {
		return false
	}

	if time.Now().After(expiry) {
		forgetSession(token)
		return false
	}

	return true
}

func forgetSession(token string) {
	globalSessionStore.mu.Lock()
	delete(globalSessionStore.sessions, token)
	delete(globalSessionStore.localOnly, token)
	delete(globalSessionStore.backendOnly, token)
	globalSessionStore.mu.Unlock()
}

func cleanupExpiredSessions() {
	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()

	now := time.Now()
	for _, sessions := range []map[string]time.Time{
		globalSessionStore.sessions,
		globalSessionStore.localOnly,
		globalSessionStore.backendOnly,
	} {
		for token, expiry := range sessions {
			if now.After(expiry) {
				delete(sessions, token)
			}
		}
	}
}

func MaskSensitive(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "..." + value[len(value)-4:]
}
