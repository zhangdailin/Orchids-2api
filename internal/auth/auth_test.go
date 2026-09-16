package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSessionBackend struct {
	sessions  map[string]time.Time
	hasErr    error
	saveErr   error
	deleted   []string
	saveCalls int
}

func (f *fakeSessionBackend) SaveSession(_ context.Context, token string, expiry time.Time) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.sessions == nil {
		f.sessions = map[string]time.Time{}
	}
	f.sessions[token] = expiry
	return nil
}

func (f *fakeSessionBackend) HasSession(_ context.Context, token string) (bool, error) {
	if f.hasErr != nil {
		return false, f.hasErr
	}
	_, ok := f.sessions[token]
	return ok, nil
}

func (f *fakeSessionBackend) DeleteSession(_ context.Context, token string) {
	delete(f.sessions, token)
	f.deleted = append(f.deleted, token)
}

// resetSessionState isolates the package-level session state between tests and
// restores whatever the process had before.
func resetSessionState(t *testing.T) {
	t.Helper()
	previousBackend := durableSessionBackend()
	clearInProcessSessions()
	SetSessionBackend(nil)
	t.Cleanup(func() {
		SetSessionBackend(previousBackend)
		clearInProcessSessions()
	})
}

func clearInProcessSessions() {
	globalSessionStore.mu.Lock()
	globalSessionStore.sessions = make(map[string]time.Time)
	globalSessionStore.localOnly = make(map[string]time.Time)
	globalSessionStore.mu.Unlock()
}

func TestGenerateSessionTokenWithoutBackend(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}
	if len(token) != sessionTokenLength*2 {
		t.Fatalf("token length = %d, want %d hex characters", len(token), sessionTokenLength*2)
	}
	if !ValidateSessionToken(token) {
		t.Fatal("a token generated in-process must validate")
	}
	if ValidateSessionToken("not-a-session") {
		t.Fatal("an unknown token must not validate")
	}
	if ValidateSessionToken("") {
		t.Fatal("an empty token must not validate")
	}
}

// A deploy restarts the process. With a durable backend the operator's cookie
// must keep working instead of bouncing the admin UI back to the login page.
func TestDurableSessionSurvivesProcessRestart(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}
	if backend.saveCalls != 1 {
		t.Fatalf("SaveSession calls = %d, want 1", backend.saveCalls)
	}

	// Simulate the restart: the new process has an empty in-process map.
	clearInProcessSessions()

	if !ValidateSessionToken(token) {
		t.Fatal("a durable session must still validate after the in-process map is dropped")
	}
}

// A backend outage must not sign out a session the process can still vouch for.
// The process keeps its own mirror of every session it issued, so an
// unreachable backend degrades to the previous in-process behaviour rather than
// locking the operator out mid-session.
func TestValidateFallsBackToInProcessWhenBackendFails(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}

	// The durable write above succeeded, so the process holds a local mirror;
	// only the backend's read path is broken now.
	SetSessionBackend(&fakeSessionBackend{hasErr: errors.New("redis unavailable")})

	if !ValidateSessionToken(token) {
		t.Fatal("a backend outage must not sign out a session this process issued")
	}
}

// A session this process never issued stays unvalidatable while the backend is
// unreachable: an outage must not become an open door for arbitrary cookies.
func TestValidateStillFailsClosedForUnknownTokensDuringAnOutage(t *testing.T) {
	resetSessionState(t)
	SetSessionBackend(&fakeSessionBackend{hasErr: errors.New("redis unavailable")})

	if ValidateSessionToken("token-from-a-previous-process") {
		t.Fatal("an unknown token must not validate while the backend is unreachable")
	}
}

// The backend has to confirm a recovered session once so a later outage can
// still accept it. Without that record the first Redis blip after a deploy signs
// the operator out again — exactly the deployment instability the durable store
// was added to remove.
func TestBackendConfirmedSessionSurvivesALaterOutage(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}

	// Restart: the in-process mirror is gone and only the backend knows the
	// session. Confirming it here is what makes the next outage survivable.
	clearInProcessSessions()
	if !ValidateSessionToken(token) {
		t.Fatal("the durable session must validate after a restart")
	}

	backend.hasErr = errors.New("redis unavailable")
	if !ValidateSessionToken(token) {
		t.Fatal("a session the backend confirmed must survive a later backend outage")
	}

	// Revocation still wins: an authoritative negative clears the record.
	backend.hasErr = nil
	delete(backend.sessions, token)
	if ValidateSessionToken(token) {
		t.Fatal("a revoked session must be rejected by a healthy backend")
	}
}

func TestValidateRejectsSessionMissingFromBackend(t *testing.T) {
	resetSessionState(t)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}

	// A session the backend never saw (for example one that was revoked while
	// the process was down) must be rejected even when the in-process mirror
	// still remembers it.
	SetSessionBackend(&fakeSessionBackend{})

	if ValidateSessionToken(token) {
		t.Fatal("a session unknown to the durable backend must be rejected")
	}
}

func TestInvalidateSessionTokenClearsBothStores(t *testing.T) {
	resetSessionState(t)
	backend := &fakeSessionBackend{}
	SetSessionBackend(backend)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}

	InvalidateSessionToken(token)

	if memoryHasSession(token) {
		t.Fatal("InvalidateSessionToken must drop the in-process session")
	}
	if _, ok := backend.sessions[token]; ok {
		t.Fatal("InvalidateSessionToken must drop the durable session")
	}
	if len(backend.deleted) != 1 || backend.deleted[0] != token {
		t.Fatalf("deleted = %v, want the invalidated token", backend.deleted)
	}
}

func TestExpiredInProcessSessionIsRejected(t *testing.T) {
	resetSessionState(t)

	rememberSession("expired-token", time.Now().Add(-time.Minute))
	if ValidateSessionToken("expired-token") {
		t.Fatal("an expired session must not validate")
	}
	if memoryHasSession("expired-token") {
		t.Fatal("an expired session should be evicted from the in-process map")
	}
}

func TestGenerateSessionTokenKeepsWorkingWhenPersistenceFails(t *testing.T) {
	resetSessionState(t)
	SetSessionBackend(&fakeSessionBackend{saveErr: errors.New("redis read-only")})

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v, want a usable local session", err)
	}
	if !ValidateSessionToken(token) {
		t.Fatal("the local session must still validate when persistence fails")
	}
}

// A session whose durable write failed is usable only until the process
// restarts; it must never be treated as if it had been persisted.
func TestLocalOnlySessionDoesNotSurviveProcessRestart(t *testing.T) {
	resetSessionState(t)
	SetSessionBackend(&fakeSessionBackend{saveErr: errors.New("redis read-only")})

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken() error = %v", err)
	}
	if !ValidateSessionToken(token) {
		t.Fatal("the local session must validate before the restart")
	}

	clearInProcessSessions()

	if ValidateSessionToken(token) {
		t.Fatal("a session that was never persisted must not survive a restart")
	}
}

func TestCleanupExpiredSessionsKeepsLiveSession(t *testing.T) {
	resetSessionState(t)

	rememberSession("live-token", time.Now().Add(time.Hour))
	rememberSession("expired-token", time.Now().Add(-time.Second))
	rememberLocalOnlySession("live-local-token", time.Now().Add(time.Hour))
	rememberLocalOnlySession("expired-local-token", time.Now().Add(-time.Second))

	cleanupExpiredSessions()

	if !memoryHasSession("live-token") {
		t.Fatal("cleanup must keep a live session")
	}
	if memoryHasSession("expired-token") {
		t.Fatal("cleanup must drop an expired session")
	}
	if !localOnlyHasSession("live-local-token") {
		t.Fatal("cleanup must keep a live local-only session")
	}
	if localOnlyHasSession("expired-local-token") {
		t.Fatal("cleanup must drop an expired local-only session")
	}
}
