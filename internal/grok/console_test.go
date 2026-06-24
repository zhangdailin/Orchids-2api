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

	_, err := h.doConsole(context.Background(), "token", map[string]interface{}{"model": "grok-build-0.1", "input": "hi"})
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

func TestServeConsoleChat_MarksAccountOn429(t *testing.T) {
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

	sess, err := h.openChatAccountSessionForModel(context.Background(), ModelSpec{ID: "grok-build-0.1", ConsoleModel: "grok-build-0.1", Tier: grokTierLite})
	if err != nil {
		t.Fatalf("open console session error=%v", err)
	}
	defer sess.Close()

	_, err = h.doConsoleWithAutoSwitch(context.Background(), sess, map[string]interface{}{"model": "grok-build-0.1", "input": "hi"})
	if err == nil {
		t.Fatal("expected 429 error")
	}

	got, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.StatusCode != "429" {
		t.Fatalf("StatusCode=%q want 429", got.StatusCode)
	}
	if got.LastAttempt.IsZero() {
		t.Fatal("LastAttempt is zero, want cooldown timestamp")
	}
}

func TestServeConsoleChat_SwitchesAccountOn429(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc1 := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=super-token-a",
		Subscription: "super",
		Weight:       1,
	}
	acc2 := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=super-token-b",
		Subscription: "super",
		Weight:       1,
	}
	basic := &store.Account{
		AccountType:  "grok",
		Enabled:      true,
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Weight:       1,
	}
	for _, acc := range []*store.Account{acc1, acc2, basic} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	seenCookies := []string{}
	failedToken := ""
	h.cfg = &config.Config{AccountSwitchCount: 2}
	h.client = &Client{
		cfg: &config.Config{},
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cookie := req.Header.Get("Cookie")
				seenCookies = append(seenCookies, cookie)
				if strings.Contains(cookie, "basic-token") {
					t.Fatalf("console switch selected basic account for super model: %s", cookie)
				}
				if len(seenCookies) == 1 {
					failedToken = cookie
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"rate limit"}`)),
						Request:    req,
					}, nil
				}
				if cookie == failedToken {
					t.Fatalf("console switch reused rate-limited account: %s", cookie)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
					Request:    req,
				}, nil
			}),
		},
	}

	sess, err := h.openChatAccountSessionForModel(context.Background(), ModelSpec{ID: "grok-build-0.1", ConsoleModel: "grok-build-0.1", Tier: grokTierSuper})
	if err != nil {
		t.Fatalf("open console session error=%v", err)
	}
	failedID := sess.acc.ID
	defer sess.Close()

	resp, err := h.doConsoleWithAutoSwitch(context.Background(), sess, map[string]interface{}{"model": "grok-build-0.1", "input": "hi"})
	if err != nil {
		t.Fatalf("doConsoleWithAutoSwitch() error = %v", err)
	}
	defer resp.Body.Close()
	if sess.acc.ID == failedID {
		t.Fatalf("switched account=%d, want a different account", sess.acc.ID)
	}
	if sess.acc.ID != acc1.ID && sess.acc.ID != acc2.ID {
		t.Fatalf("switched account=%d, want one of the super accounts", sess.acc.ID)
	}
	if len(seenCookies) != 2 {
		t.Fatalf("request count=%d want 2", len(seenCookies))
	}

	got, err := s.GetAccount(context.Background(), failedID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.StatusCode != "429" {
		t.Fatalf("failed account StatusCode=%q want 429", got.StatusCode)
	}
}
