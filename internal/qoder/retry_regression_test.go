package qoder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestRefreshReplayKeepsIndependentTransientBudget(t *testing.T) {
	for _, statuses := range [][]int{
		{500, 401, 200},
		{500, 500, 401, 200},
		{401, 500, 500, 200},
		{401, 401},
		{500, 500, 500},
	} {
		t.Run(fmt.Sprint(statuses), func(t *testing.T) {
			t.Parallel()
			var chats, refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "deviceToken/refresh") {
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`)
					return
				}
				n := int(chats.Add(1)) - 1
				if n >= len(statuses) {
					t.Errorf("unexpected extra chat attempt %d", n+1)
					w.WriteHeader(400)
					return
				}
				switch statuses[n] {
				case 500:
					w.WriteHeader(500)
					_, _ = io.WriteString(w, `{"message":"provider_error"}`)
				case 401:
					w.WriteHeader(401)
					_, _ = io.WriteString(w, `{"message":"login expired"}`)
				default:
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
				}
			}))
			defer server.Close()
			client := NewFromAccount(signedTestAccount(), nil)
			setTestEndpoints(client, server.URL, server.URL, server.URL)
			err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}}}, nil, nil)
			success := statuses[len(statuses)-1] == 200
			if (err == nil) != success {
				t.Fatalf("error=%v success wanted=%v", err, success)
			}
			if int(chats.Load()) != len(statuses) {
				t.Fatalf("chat calls=%d want %d", chats.Load(), len(statuses))
			}
			wantRefresh := int32(0)
			for _, s := range statuses {
				if s == 401 {
					wantRefresh = 1
				}
			}
			if refreshes.Load() != wantRefresh {
				t.Fatalf("refresh calls=%d want %d", refreshes.Load(), wantRefresh)
			}
		})
	}
}

type backoffCancelTransport struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (tr *backoffCancelTransport) RoundTrip(*http.Request) (*http.Response, error) {
	tr.calls.Add(1)
	return &http.Response{StatusCode: 500, Header: make(http.Header), Body: &cancelOnCloseBody{Reader: strings.NewReader(`{"message":"provider_error"}`), cancel: tr.cancel}}, nil
}

type cancelOnCloseBody struct {
	*strings.Reader
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnCloseBody) Close() error { b.once.Do(b.cancel); return nil }

func TestTransientBackoffReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewFromAccount(signedTestAccount(), nil)
	transport := &backoffCancelTransport{cancel: cancel}
	client.stream = &http.Client{Transport: transport}
	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "Qwen3.7-Max", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}}}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want context.Canceled", err)
	}
	if transport.calls.Load() != 1 {
		t.Fatalf("calls=%d want 1", transport.calls.Load())
	}
}
