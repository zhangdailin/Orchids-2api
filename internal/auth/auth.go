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
}

// globalSessionStore mirrors every session this process issued. It is the
// authoritative store when no durable backend is configured.
var globalSessionStore = &SessionStore{
	sessions:  make(map[string]time.Time),
	localOnly: make(map[string]time.Time),
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
			return valid
		}
		// A backend outage must not sign out a session this process issued.
		slog.Warn("Admin session backend unavailable; using the in-process store", "error", err)
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
	globalSessionStore.mu.Unlock()
}

func cleanupExpiredSessions() {
	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()

	now := time.Now()
	for token, expiry := range globalSessionStore.sessions {
		if now.After(expiry) {
			delete(globalSessionStore.sessions, token)
		}
	}
	for token, expiry := range globalSessionStore.localOnly {
		if now.After(expiry) {
			delete(globalSessionStore.localOnly, token)
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
