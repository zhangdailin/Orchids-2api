package orchids

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func encodeJWTClaims(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func TestGetTokenPrefersStoredAccountToken(t *testing.T) {
	prevFetch := orchidsFetchClerkInfoWithSession
	t.Cleanup(func() {
		orchidsFetchClerkInfoWithSession = prevFetch
	})

	orchidsFetchClerkInfoWithSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		t.Fatal("expected stored Orchids token to be used before Clerk fetch")
		return nil, nil
	}

	token := "header." + encodeJWTClaims(`{"sid":"sess_123","sub":"user_123","exp":4102444800}`) + ".sig"
	client := NewFromAccount(&store.Account{
		ID:           1,
		AccountType:  "orchids",
		ClientCookie: "client-cookie",
		SessionID:    "sess_123",
		Token:        token,
	}, &config.Config{})

	got, err := client.GetToken()
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("GetToken()=%q want %q", got, token)
	}
}

func TestGetTokenUsesClerkJWTWithoutTokensEndpoint(t *testing.T) {
	prevFetch := orchidsFetchClerkInfoWithSession
	t.Cleanup(func() {
		orchidsFetchClerkInfoWithSession = prevFetch
	})

	token := "header." + encodeJWTClaims(`{"sid":"sess_456","sub":"user_456","exp":4102444800}`) + ".sig"
	orchidsFetchClerkInfoWithSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		if clientUat != "" {
			t.Fatalf("clientUat=%q want empty", clientUat)
		}
		if sessionID != "" {
			t.Fatalf("sessionID=%q want empty", sessionID)
		}
		return &clerk.AccountInfo{
			SessionID:    "sess_456",
			ClientCookie: "rotated-client-cookie",
			ClientUat:    "1234567890",
			UserID:       "user_456",
			Email:        "user@example.com",
			JWT:          token,
		}, nil
	}

	client := NewFromAccount(&store.Account{
		ID:           2,
		AccountType:  "orchids",
		ClientCookie: "client-cookie",
	}, &config.Config{})
	client.httpClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("unexpected network call to %s", req.URL.String())
		}),
	}

	got, err := client.GetToken()
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if got != token {
		t.Fatalf("GetToken()=%q want %q", got, token)
	}
	if client.account.Token != token {
		t.Fatalf("account token not persisted, got %q", client.account.Token)
	}
	if client.account.ClientCookie != "rotated-client-cookie" {
		t.Fatalf("client cookie not rotated, got %q", client.account.ClientCookie)
	}
}

func TestGetTokenDoesNotDoubleHitClerkAfterAccountFetch429(t *testing.T) {
	prevFetch := orchidsFetchClerkInfoWithSession
	prevRefresh := orchidsFetchClerkInfoWithProjectAndSession
	t.Cleanup(func() {
		orchidsFetchClerkInfoWithSession = prevFetch
		orchidsFetchClerkInfoWithProjectAndSession = prevRefresh
	})

	orchidsFetchClerkInfoWithSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		return nil, fmt.Errorf("unexpected status code 429: too many requests")
	}
	orchidsFetchClerkInfoWithProjectAndSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, customProjectID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		t.Fatal("expected GetToken to avoid a second Clerk refresh call")
		return nil, nil
	}

	client := NewFromAccount(&store.Account{
		ID:           3,
		AccountType:  "orchids",
		ClientCookie: "client-cookie",
	}, &config.Config{AutoRefreshToken: true})
	client.httpClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected network call to %s", req.URL.String())
			return nil, nil
		}),
	}

	got, err := client.GetToken()
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("GetToken() error = %v, want 429 error", err)
	}
	if got != "" {
		t.Fatalf("GetToken()=%q want empty token", got)
	}
}

func TestGetTokenDoesNotFallbackToSessionTokensEndpointWhenSessionKnown(t *testing.T) {
	prevFetch := orchidsFetchClerkInfoWithSession
	prevRefresh := orchidsFetchClerkInfoWithProjectAndSession
	t.Cleanup(func() {
		orchidsFetchClerkInfoWithSession = prevFetch
		orchidsFetchClerkInfoWithProjectAndSession = prevRefresh
	})

	orchidsFetchClerkInfoWithSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		return nil, fmt.Errorf("unexpected status code 429: too many requests")
	}
	orchidsFetchClerkInfoWithProjectAndSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, customProjectID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		t.Fatal("expected GetToken to avoid a second Clerk refresh call")
		return nil, nil
	}

	client := NewFromAccount(&store.Account{
		ID:           4,
		AccountType:  "orchids",
		ClientCookie: "client-cookie",
		SessionID:    "sess_789",
		ClientUat:    "1739251200",
	}, &config.Config{AutoRefreshToken: true})
	client.httpClient = &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected session token endpoint call to %s", req.URL.String())
			return nil, nil
		}),
	}

	got, err := client.GetToken()
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("GetToken() error = %v, want 429 error", err)
	}
	if got != "" {
		t.Fatalf("GetToken()=%q want empty token", got)
	}
}

func TestBootstrapClientCookieFromSessionCapturesSetCookie(t *testing.T) {
	client := &Client{
		config: &config.Config{
			SessionID:      "sess_bootstrap",
			SessionCookie:  "session-cookie",
			RequestTimeout: 5,
		},
		account: &store.Account{},
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/v1/client" {
					t.Fatalf("path=%q want /v1/client", req.URL.Path)
				}
				if got := req.Header.Get("Cookie"); !strings.Contains(got, "__session=session-cookie") || !strings.Contains(got, "clerk_active_context=sess_bootstrap:") {
					t.Fatalf("cookie=%q want session + active context", got)
				}
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"response":{"sessions":[]}}`)),
					Header:     make(http.Header),
				}
				resp.Header.Add("Set-Cookie", "__client=bootstrapped-client; Path=/; HttpOnly")
				resp.Header.Add("Set-Cookie", "__client_uat=1773712060; Path=/")
				return resp, nil
			}),
		},
	}

	if err := client.bootstrapClientCookieFromSession(); err != nil {
		t.Fatalf("bootstrapClientCookieFromSession() error = %v", err)
	}
	if client.config.ClientCookie != "bootstrapped-client" {
		t.Fatalf("ClientCookie=%q want bootstrapped-client", client.config.ClientCookie)
	}
	if client.config.ClientUat != "1773712060" {
		t.Fatalf("ClientUat=%q want 1773712060", client.config.ClientUat)
	}
}

func TestGetChatTokenRejectsSessionOnlyAccountWithoutActiveClerkSession(t *testing.T) {
	prevFetch := orchidsFetchClerkInfoWithSession
	t.Cleanup(func() {
		orchidsFetchClerkInfoWithSession = prevFetch
	})

	orchidsFetchClerkInfoWithSession = func(clientCookie string, sessionCookie string, clientUat string, sessionID string, proxyFunc func(*http.Request) (*url.URL, error)) (*clerk.AccountInfo, error) {
		if clientCookie != "bootstrapped-client" {
			t.Fatalf("clientCookie=%q want bootstrapped-client", clientCookie)
		}
		if clientUat != "1773712060" {
			t.Fatalf("clientUat=%q want 1773712060", clientUat)
		}
		if sessionID != "sess_bootstrap" {
			t.Fatalf("sessionID=%q want sess_bootstrap", sessionID)
		}
		return nil, fmt.Errorf("no active sessions found")
	}

	client := &Client{
		config: &config.Config{
			SessionID:      "sess_bootstrap",
			SessionCookie:  "session-cookie",
			RequestTimeout: 5,
		},
		account: &store.Account{
			ID:            7,
			AccountType:   "orchids",
			SessionID:     "sess_bootstrap",
			SessionCookie: "session-cookie",
			Token:         "session-cookie",
		},
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/v1/client" {
					t.Fatalf("path=%q want /v1/client", req.URL.Path)
				}
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"response":{"sessions":[]}}`)),
					Header:     make(http.Header),
				}
				resp.Header.Add("Set-Cookie", "__client=bootstrapped-client; Path=/; HttpOnly")
				resp.Header.Add("Set-Cookie", "__client_uat=1773712060; Path=/")
				return resp, nil
			}),
		},
	}

	_, err := client.getChatToken()
	if err == nil || !strings.Contains(err.Error(), "no active sessions found") {
		t.Fatalf("getChatToken() error = %v, want no active sessions found", err)
	}
	if client.config.ClientCookie != "bootstrapped-client" {
		t.Fatalf("ClientCookie=%q want bootstrapped-client", client.config.ClientCookie)
	}
}
