package grok

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestUnifiedWorkerPoolBoundedAndExactlyOnce(t *testing.T) {
	const workers, count = 4, 100
	items := make([]int, count)
	seen := make([]atomic.Int32, count)
	for i := range items {
		items[i] = i
	}
	var active, peak atomic.Int32
	entered := make(chan struct{}, workers)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runWorkerPool(context.Background(), items, workers, func(i int) {
			n := active.Add(1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			if i < workers {
				entered <- struct{}{}
				<-release
			}
			seen[i].Add(1)
			active.Add(-1)
		}, nil)
		close(done)
	}()
	for i := 0; i < workers; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("workers failed to start")
		}
	}
	select {
	case <-done:
		t.Error("pool returned before its workers")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool did not finish")
	}
	if peak.Load() != workers || active.Load() != 0 {
		t.Fatalf("peak=%d active=%d", peak.Load(), active.Load())
	}
	for i := range seen {
		if seen[i].Load() != 1 {
			t.Errorf("item %d processed %d times", i, seen[i].Load())
		}
	}
}

func TestUnifiedWorkerPoolCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var processed, canceled atomic.Int32
	items := []int{1, 2, 3, 4, 5}
	process := func(int) { processed.Add(1) }
	runWorkerPool(ctx, items, 2, process, func(int) { canceled.Add(1) })
	runWorkerPool(ctx, items, 2, process, nil)
	if processed.Load() != 0 || canceled.Load() != int32(len(items)) {
		t.Fatalf("processed=%d canceled=%d", processed.Load(), canceled.Load())
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	canceled.Store(0)
	runWorkerPool(ctx, items, 1, func(int) { processed.Add(1); cancel() }, func(int) { canceled.Add(1) })
	if processed.Load() != 1 || canceled.Load() != 4 {
		t.Fatalf("in-flight cancellation: processed=%d canceled=%d", processed.Load(), canceled.Load())
	}
}

func TestUnifiedCanceledAdminBatchesReportEveryItem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil handler makes any accidental upstream/store access fail this test.
	var h *Handler
	var nsfwCalls, refreshCalls atomic.Int32
	targets := []nsfwTarget{{Token: "one"}, {Token: "two"}, {Token: "three"}}
	ok, results := h.runNSFWEnableBatch(ctx, targets, nil, 2, func(_ string, result nsfwItemResult) {
		nsfwCalls.Add(1)
		if result.Success || result.Error != context.Canceled.Error() {
			t.Errorf("unexpected canceled NSFW result: %+v", result)
		}
	})
	refreshed := h.runTokenRefreshBatch(ctx, []string{"one", "two", "three"}, "", nil, 2, func(_ string, success bool) {
		refreshCalls.Add(1)
		if success {
			t.Error("canceled refresh succeeded")
		}
	})
	if ok != 0 || len(results) != 3 || len(refreshed) != 3 || nsfwCalls.Load() != 3 || refreshCalls.Load() != 3 {
		t.Fatalf("incomplete results: ok=%d nsfw=%v refresh=%v callbacks=%d/%d", ok, results, refreshed, nsfwCalls.Load(), refreshCalls.Load())
	}
}

func TestUnifiedHTTPResponseLifecycle(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://upstream.test", nil)
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write([]byte(`{"message":"preserved"}`))
			_ = writer.Close()
			released := 0
			resp, err := doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Encoding": {"gzip"}, "Retry-After": {"7"}}, Body: &leaseResponseBody{ReadCloser: io.NopCloser(&compressed), release: func() { released++ }}}, nil
			}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(body) != `{"message":"preserved"}` || resp.StatusCode != status || resp.Header.Get("Retry-After") != "7" || released != 1 || resp.Header.Get("Content-Encoding") != "" {
				t.Fatalf("body=%s status=%d released=%d error=%v", body, resp.StatusCode, released, err)
			}
		})
	}
	released := 0
	resp, err := doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: &leaseResponseBody{ReadCloser: io.NopCloser(strings.NewReader("invalid gzip")), release: func() { released++ }}}, nil
	}, 0)
	if resp != nil || err == nil || released != 1 {
		t.Fatalf("decode failure leaked response: resp=%v err=%v released=%d", resp, err, released)
	}
	wantErr := errors.New("transport failure")
	_, err = doUpstreamHTTP(req, func(*http.Request) (*http.Response, error) { return nil, wantErr }, 0)
	if !errors.Is(err, wantErr) {
		t.Fatalf("transport error changed: %v", err)
	}
}

func TestUnifiedBuildRequestPaths(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("x-grok-user-id") != "test-user" || r.Header.Get("x-userid") != "test-user" {
			t.Errorf("missing shared authentication/identity headers")
		}
		switch r.URL.Path {
		case "/fallback/videos":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("fallback POST metadata lost")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"prompt":"test"}` {
				t.Errorf("fallback payload=%s", body)
			}
			w.WriteHeader(http.StatusAccepted)
		case "/fallback/videos/job":
			if r.Method != http.MethodGet || r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
				t.Error("fallback GET must not acquire POST metadata")
			}
		case "/v1/videos/job":
			if r.URL.RawQuery != "include=output&after=2" || r.Header.Get("x-grok-model-override") != "grok-imagine-video-1.5" {
				t.Error("resource query/model override lost")
			}
			w.Header().Set("X-Resource-Status", "missing")
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"job"}`)
	}))
	defer server.Close()
	cfg := &config.Config{GrokCLIBaseURL: server.URL + "/v1", GrokCLIFallbackBaseURL: server.URL + "/fallback"}
	c := &CLIClient{cfg: cfg, httpClient: server.Client(), oauth: NewCLIOAuth(cfg, server.Client())}
	acc := &store.Account{OAuthAccessToken: "test-token", OAuthExpiresAt: time.Now().Add(time.Hour), UserID: "test-user"}
	for _, tc := range []struct {
		method, path string
		payload      map[string]interface{}
		status       int
	}{
		{http.MethodPost, "/videos", map[string]interface{}{"prompt": "test"}, http.StatusAccepted},
		{http.MethodGet, "/videos/job", nil, http.StatusOK},
	} {
		resp, err := c.doFallbackRequest(context.Background(), acc, tc.method, tc.path, tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("fallback status=%d want=%d", resp.StatusCode, tc.status)
		}
	}
	resp, err := c.doResponseResource(context.Background(), acc, http.MethodGet, "/videos/job", "include=output&after=2")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Resource-Status") != "missing" || string(body) != `{"id":"job"}` || calls.Load() != 3 {
		t.Fatalf("resource response changed: status=%d body=%s calls=%d", resp.StatusCode, body, calls.Load())
	}
}
