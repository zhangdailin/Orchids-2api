//go:build legacy_console

package grok

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func resetConsoleTeamCooldownForTest(t *testing.T) {
	t.Helper()
	clear := func() {
		consoleTeamCooldownMu.Lock()
		consoleTeamCooldownUntil = time.Time{}
		consoleTeamCooldownMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

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
	if got.StatusCode != "429" {
		t.Fatalf("StatusCode=%q want 429", got.StatusCode)
	}
	if got.LastAttempt.IsZero() {
		t.Fatal("LastAttempt is zero, want cooldown timestamp")
	}
}

func TestServeConsoleChat_DoesNotSwitchOrMarkOnConsoleTeam429(t *testing.T) {
	resetConsoleTeamCooldownForTest(t)

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
	for _, acc := range []*store.Account{acc1, acc2} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}

	calls := 0
	h.cfg = &config.Config{AccountSwitchCount: 2}
	h.client = &Client{
		cfg: &config.Config{},
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						`{"code":"resource-exhausted","error":"Too many requests for team 00000000-0000-0000-0000-000000000013 and model grok-4.3. Your team's rate limit is - Requests per Second (actual/limit): 2/2, Requests per Minute (actual/limit): 73/60."}`,
					)),
					Request: req,
				}, nil
			}),
		},
	}

	sess, err := h.openChatAccountSessionForModel(context.Background(), ModelSpec{ID: "grok-4.3", ConsoleModel: "grok-4.3", Tier: grokTierSuper})
	if err != nil {
		t.Fatalf("open console session error=%v", err)
	}
	firstID := sess.acc.ID
	defer sess.Close()

	_, err = h.doConsoleWithAutoSwitch(context.Background(), sess, map[string]interface{}{"model": "grok-4.3", "input": "hi"})
	if err == nil {
		t.Fatal("expected 429 error")
	}
	if sess.acc.ID != firstID {
		t.Fatalf("switched account=%d, want original account %d", sess.acc.ID, firstID)
	}
	if calls != 1 {
		t.Fatalf("request count=%d want 1", calls)
	}

	for _, acc := range []*store.Account{acc1, acc2} {
		got, err := s.GetAccount(context.Background(), acc.ID)
		if err != nil {
			t.Fatalf("GetAccount(%d) error = %v", acc.ID, err)
		}
		if got.StatusCode != "" {
			t.Fatalf("account %d StatusCode=%q want empty", acc.ID, got.StatusCode)
		}
		if !got.LastAttempt.IsZero() {
			t.Fatalf("account %d LastAttempt=%v want zero", acc.ID, got.LastAttempt)
		}
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

	sess, err := h.openChatAccountSessionForModel(context.Background(), ModelSpec{ID: "grok-4.3", ConsoleModel: "grok-4.3", Tier: grokTierSuper})
	if err != nil {
		t.Fatalf("open console session error=%v", err)
	}
	failedID := sess.acc.ID
	defer sess.Close()

	resp, err := h.doConsoleWithAutoSwitch(context.Background(), sess, map[string]interface{}{"model": "grok-4.3", "input": "hi"})
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
