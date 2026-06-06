package grok

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestDoConsole_DoesNotRetry429OnSameAccount(t *testing.T) {
	t.Parallel()

	calls := 0
	h := &Handler{
		client: &Client{
			cfg: &config.Config{
				MaxRetries:       3,
				Retry429Interval: 30,
			},
			httpClient: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"rate limit"}`)),
						Request:    req,
					}, nil
				}),
			},
		},
	}

	_, err := h.doConsole(context.Background(), "token", map[string]interface{}{"model": "grok-4.3", "input": "hi"})
	if err == nil {
		t.Fatal("expected doConsole() to fail on 429")
	}
	if !strings.Contains(err.Error(), "grok upstream status=429") {
		t.Fatalf("error=%q, want 429 upstream error", err.Error())
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

func TestServeConsoleChat_DoesNotMarkAccountOn429(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=lite-token",
		Subscription: "lite",
		Weight:       1,
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	h.client = &Client{
		cfg: &config.Config{},
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate limit"}`)),
					Request:    req,
				}, nil
			}),
		},
	}

	sess, err := h.openChatAccountSessionForModel(context.Background(), ModelSpec{ID: "grok-4.3", ConsoleModel: "grok-4.3", Tier: grokTierLite})
	if err != nil {
		t.Fatalf("open console session error=%v", err)
	}
	defer sess.Close()

	_, err = h.doConsoleWithAutoSwitch(context.Background(), sess, map[string]interface{}{"model": "grok-4.3", "input": "hi"})
	if err == nil {
		t.Fatal("expected 429 error")
	}

	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.StatusCode != "" {
		t.Fatalf("StatusCode=%q want empty", got.StatusCode)
	}
	if !got.LastAttempt.IsZero() {
		t.Fatalf("LastAttempt=%v want zero", got.LastAttempt)
	}
}
