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
