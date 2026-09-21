package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestDPoPSessionManagerCleansExpiredSessions(t *testing.T) {
	manager := newDPoPSessionManager()
	now := time.Now().UTC()
	manager.sessions["expired"] = dpopSession{accessToken: "expired", expiresAt: now.Add(dpopRefreshSkew)}
	manager.sessions["valid"] = dpopSession{accessToken: "valid", expiresAt: now.Add(time.Minute)}
	manager.lastCleanup = now.Add(-dpopSessionCleanupPeriod)

	if session, ok := manager.cached("valid"); !ok || session.accessToken != "valid" {
		t.Fatalf("valid session missing: ok=%v session=%+v", ok, session)
	}
	manager.mu.Lock()
	_, expiredExists := manager.sessions["expired"]
	manager.mu.Unlock()
	if expiredExists {
		t.Fatal("expired session survived global cleanup")
	}
}

func TestDPoPSessionManagerCapacityBound(t *testing.T) {
	manager := newDPoPSessionManager()
	now := time.Now().UTC()
	for i := 0; i < maxDPoPSessions+128; i++ {
		manager.store(string(rune(i)), dpopSession{
			accessToken: "token",
			expiresAt:   now.Add(time.Hour + time.Duration(i)*time.Second),
		})
	}
	manager.mu.Lock()
	got := len(manager.sessions)
	manager.mu.Unlock()
	if got != maxDPoPSessions {
		t.Fatalf("sessions=%d want %d", got, maxDPoPSessions)
	}
}

func TestDPoPSessionManagerConcurrentCapacityBound(t *testing.T) {
	manager := newDPoPSessionManager()
	now := time.Now().UTC()
	const workers = 16
	const entriesPerWorker = 128
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := 0; entry < entriesPerWorker; entry++ {
				key := string(rune(worker*entriesPerWorker + entry))
				manager.store(key, dpopSession{accessToken: key, expiresAt: now.Add(time.Hour)})
				manager.cached(key)
			}
		}()
	}
	wg.Wait()
	manager.mu.Lock()
	got := len(manager.sessions)
	manager.mu.Unlock()
	if got > maxDPoPSessions {
		t.Fatalf("sessions=%d exceeds limit %d", got, maxDPoPSessions)
	}
}

func TestDPoPSessionCoalescesConcurrentFetches(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &Client{
		dpop: newDPoPSessionManager(),
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			var request struct {
				JWK dpopJWK `json:"jwk"`
			}
			raw, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			thumbprint, err := dpopJWKThumbprint(request.JWK)
			if err != nil {
				return nil, err
			}
			claims, _ := json.Marshal(map[string]interface{}{
				"exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]interface{}{"jkt": thumbprint},
			})
			access := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
			body, _ := json.Marshal(map[string]interface{}{
				"access_token": access, "token_type": "DPoP", "expires_in": 3600,
			})
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}

	const workers = 16
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := client.dpopSession(context.Background(), "console-token")
			errs <- err
		}()
	}
	close(start)
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
		t.Fatalf("DPoP token fetches=%d want 1", got)
	}
}
