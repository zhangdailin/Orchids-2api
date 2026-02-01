package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	sessionTokenLength = 32
	sessionTTL         = 7 * 24 * time.Hour
)

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
}

var globalSessionStore = &SessionStore{
	sessions: make(map[string]time.Time),
}

func GenerateSessionToken() (string, error) {
	bytes := make([]byte, sessionTokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	token := hex.EncodeToString(bytes)
	
	globalSessionStore.mu.Lock()
	globalSessionStore.sessions[token] = time.Now().Add(sessionTTL)
	globalSessionStore.mu.Unlock()
	
	return token, nil
}

func ValidateSessionToken(token string) bool {
	globalSessionStore.mu.RLock()
	expiry, exists := globalSessionStore.sessions[token]
	globalSessionStore.mu.RUnlock()
	
	if !exists {
		return false
	}
	
	if time.Now().After(expiry) {
		globalSessionStore.mu.Lock()
		delete(globalSessionStore.sessions, token)
		globalSessionStore.mu.Unlock()
		return false
	}
	
	return true
}

func InvalidateSessionToken(token string) {
	globalSessionStore.mu.Lock()
	delete(globalSessionStore.sessions, token)
	globalSessionStore.mu.Unlock()
}

func CleanupExpiredSessions() {
	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()
	
	now := time.Now()
	for token, expiry := range globalSessionStore.sessions {
		if now.After(expiry) {
			delete(globalSessionStore.sessions, token)
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
