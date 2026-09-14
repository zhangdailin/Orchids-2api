package warp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRefresh_UsesFirebaseWhenSuccessful(t *testing.T) {
	t.Parallel()

	sess := &session{refreshToken: "token-123"}
	firebaseTokenURL := "https://firebase.test/v1/token?key=test-key"
	var seen string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen = req.URL.String()
			if req.URL.String() != firebaseTokenURL {
				t.Fatalf("unexpected refresh URL: %s", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(`{"id_token":"firebase-jwt","refresh_token":"rotated-token","expires_in":"3600"}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	if err := sess.refresh(context.Background(), client, firebaseTokenURL); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if seen != firebaseTokenURL {
		t.Fatalf("endpoint=%q want %q", seen, firebaseTokenURL)
	}
	if sess.currentJWT() != "firebase-jwt" {
		t.Fatalf("currentJWT=%q want firebase-jwt", sess.currentJWT())
	}
	if sess.currentRefreshToken() != "rotated-token" {
		t.Fatalf("currentRefreshToken=%q want rotated-token", sess.currentRefreshToken())
	}
}

func TestEnsureLoginCoalescesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	})}
	sess := &session{jwt: "jwt", expiresAt: time.Now().Add(time.Hour)}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sess.ensureLogin(context.Background(), client)
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("login calls=%d want 1", got)
	}
}

func TestEnsureLoginRetriesImmediatelyAfterFailure(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		if calls.Add(1) == 1 {
			status = http.StatusBadGateway
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	})}
	sess := &session{jwt: "jwt", expiresAt: time.Now().Add(time.Hour)}
	if err := sess.ensureLogin(context.Background(), client); err == nil {
		t.Fatal("first login unexpectedly succeeded")
	}
	if err := sess.ensureLogin(context.Background(), client); err != nil {
		t.Fatalf("second login should retry immediately: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("login calls=%d want 2", got)
	}
}

func TestSessionRefresh_DoesNotFallbackToWarpTokenProxy(t *testing.T) {
	t.Parallel()

	sess := &session{refreshToken: "token-123"}
	firebaseTokenURL := "https://firebase.test/v1/token?key=test-key"
	var seen []string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			seen = append(seen, req.URL.String())
			switch req.URL.String() {
			case firebaseTokenURL:
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Body:       io.NopCloser(bytes.NewBufferString(`{"error":"firebase unavailable"}`)),
					Header:     make(http.Header),
				}, nil
			default:
				t.Fatalf("unexpected refresh URL: %s", req.URL.String())
				return nil, nil
			}
		}),
	}

	if err := sess.refresh(context.Background(), client, firebaseTokenURL); err == nil {
		t.Fatal("expected refresh error")
	}
	if len(seen) != 1 || seen[0] != firebaseTokenURL {
		t.Fatalf("seen refresh URLs=%v", seen)
	}
	if sess.currentJWT() != "" {
		t.Fatalf("currentJWT=%q want empty", sess.currentJWT())
	}
	if sess.currentRefreshToken() != "token-123" {
		t.Fatalf("currentRefreshToken=%q want token-123", sess.currentRefreshToken())
	}
}
