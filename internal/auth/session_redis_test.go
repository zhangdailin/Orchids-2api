package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedisSessionBackendForTest(t *testing.T) (*miniredis.Miniredis, SessionBackend) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	backend := NewRedisSessionBackend(client, "test")
	if backend == nil {
		t.Fatal("NewRedisSessionBackend() returned nil for a live client")
	}
	return mini, backend
}

func TestNewRedisSessionBackendRequiresClient(t *testing.T) {
	if backend := NewRedisSessionBackend(nil, "test"); backend != nil {
		t.Fatal("NewRedisSessionBackend(nil) must return nil so the in-process store stays in place")
	}
}

func TestRedisSessionBackendRoundTrip(t *testing.T) {
	_, backend := newRedisSessionBackendForTest(t)
	ctx := context.Background()

	ok, err := backend.HasSession(ctx, "unknown-token")
	if err != nil {
		t.Fatalf("HasSession(unknown) error = %v", err)
	}
	if ok {
		t.Fatal("an unknown token must not be valid")
	}

	if err := backend.SaveSession(ctx, "token-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}
	ok, err = backend.HasSession(ctx, "token-1")
	if err != nil || !ok {
		t.Fatalf("HasSession(saved) = %v, %v; want true, nil", ok, err)
	}

	backend.DeleteSession(ctx, "token-1")
	ok, err = backend.HasSession(ctx, "token-1")
	if err != nil || ok {
		t.Fatalf("HasSession(deleted) = %v, %v; want false, nil", ok, err)
	}
}

func TestRedisSessionBackendExpiresWithTheSession(t *testing.T) {
	mini, backend := newRedisSessionBackendForTest(t)
	ctx := context.Background()

	if err := backend.SaveSession(ctx, "token-1", time.Now().Add(90*time.Second)); err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}
	mini.FastForward(3 * time.Minute)

	ok, err := backend.HasSession(ctx, "token-1")
	if err != nil {
		t.Fatalf("HasSession() error = %v", err)
	}
	if ok {
		t.Fatal("an expired session must not validate")
	}
}

// Persisting an already expired session must fail loudly: reporting success
// would let the caller keep a token in its process-local mirror that the durable
// store never accepted, leaving a session that is valid here and nowhere else.
func TestRedisSessionBackendRejectsAlreadyExpiredSessions(t *testing.T) {
	mini, backend := newRedisSessionBackendForTest(t)
	ctx := context.Background()

	if err := backend.SaveSession(ctx, "token-1", time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("SaveSession() of an expired session must return an error")
	}
	if len(mini.Keys()) != 0 {
		t.Fatalf("keys = %v, want no key written for an expired session", mini.Keys())
	}
}

// A Redis dump must never hand out a usable cookie, so the raw token stays out
// of both the key and the value.
func TestRedisSessionBackendStoresOnlyADigest(t *testing.T) {
	mini, backend := newRedisSessionBackendForTest(t)
	const token = "plain-token-that-must-not-be-stored"

	if err := backend.SaveSession(context.Background(), token, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SaveSession() error = %v", err)
	}

	keys := mini.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want exactly one session key", keys)
	}
	if !strings.HasPrefix(keys[0], "test:admin:sessions:") {
		t.Fatalf("key = %q, want the configured prefix", keys[0])
	}
	if strings.Contains(keys[0], token) {
		t.Fatalf("key %q must not embed the raw session token", keys[0])
	}
	value, err := mini.Get(keys[0])
	if err != nil {
		t.Fatalf("Get(%q) error = %v", keys[0], err)
	}
	if strings.Contains(value, token) {
		t.Fatalf("value %q must not embed the raw session token", value)
	}
}
