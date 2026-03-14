package warp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewFromAccount_ProxyAppliedToEachAccount(t *testing.T) {
	base := &config.Config{
		ProxyHTTP: "http://proxy.local:3128",
		ProxyUser: "user",
		ProxyPass: "pass",
	}

	accounts := []*store.Account{
		{ID: 1, RefreshToken: "token-1"},
		{ID: 2, RefreshToken: "token-2"},
	}

	for _, acc := range accounts {
		client := NewFromAccount(acc, base)
		if client == nil || client.httpClient == nil {
			t.Fatalf("expected http client for account %d", acc.ID)
		}
		var proxyFunc func(*http.Request) (*url.URL, error)
		switch transport := client.httpClient.Transport.(type) {
		case *warpTransport:
			proxyFunc = transport.proxyFunc
		default:
			t.Fatalf("unexpected transport type for account %d: %T", acc.ID, client.httpClient.Transport)
		}
		if proxyFunc == nil {
			t.Fatalf("expected proxy func for account %d", acc.ID)
		}
		proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}})
		if err != nil {
			t.Fatalf("proxy func failed for account %d: %v", acc.ID, err)
		}
		if proxyURL == nil {
			t.Fatalf("proxy url is nil for account %d", acc.ID)
		}
		if proxyURL.Host != "proxy.local:3128" {
			t.Fatalf("unexpected proxy host for account %d: %s", acc.ID, proxyURL.Host)
		}
		if proxyURL.User == nil || proxyURL.User.Username() != "user" {
			t.Fatalf("unexpected proxy user for account %d", acc.ID)
		}
		if pass, ok := proxyURL.User.Password(); !ok || pass != "pass" {
			t.Fatalf("unexpected proxy password for account %d", acc.ID)
		}
	}
}

func TestNewFromAccount_LegacyWarpClientsShareCookieJar(t *testing.T) {
	client := NewFromAccount(&store.Account{ID: 7, RefreshToken: "token-7"}, &config.Config{})
	if client == nil {
		t.Fatal("expected client")
	}
	if client.authClient == nil {
		t.Fatal("expected auth client")
	}
	if client.httpClient == nil {
		t.Fatal("expected stream client")
	}
	if client.httpClient.Jar == nil || client.authClient.Jar == nil {
		t.Fatal("expected legacy Warp clients to keep shared cookie jars")
	}
	if client.httpClient.Jar != client.authClient.Jar {
		t.Fatal("expected stream and auth clients to share cookie jar")
	}
}

func TestNewHTTPClient_ProxyBypassAndHTTPS(t *testing.T) {
	cfg := &config.Config{
		ProxyHTTP:   "http://proxy.local:3128",
		ProxyHTTPS:  "http://secure.proxy:8443",
		ProxyUser:   "user",
		ProxyPass:   "pass",
		ProxyBypass: []string{"warp.com"},
	}

	client := newHTTPClient(0, cfg)
	if client == nil || client.Transport == nil {
		t.Fatal("expected http client transport")
	}
	transport, ok := client.Transport.(*warpTransport)
	if !ok || transport == nil {
		t.Fatal("expected warp transport")
	}
	if transport.proxyFunc == nil {
		t.Fatal("expected proxy func")
	}

	bypassReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "warp.com"}}
	proxyURL, err := transport.proxyFunc(bypassReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("expected bypass to skip proxy, got %v", proxyURL)
	}

	httpsReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	proxyURL, err = transport.proxyFunc(httpsReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "secure.proxy:8443" {
		t.Fatalf("unexpected https proxy: %v", proxyURL)
	}

	httpReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}
	proxyURL, err = transport.proxyFunc(httpReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.local:3128" {
		t.Fatalf("unexpected http proxy: %v", proxyURL)
	}
}

func TestDoStreamRequest_SendsLegacyWarpHeaders(t *testing.T) {
	client := NewFromAccount(&store.Account{ID: 1, RefreshToken: "token-1"}, &config.Config{})
	client.session.mu.Lock()
	client.session.jwt = "test-jwt"
	client.session.expiresAt = time.Now().Add(time.Hour)
	client.session.mu.Unlock()

	var seenAccept string
	var seenClientID string
	client.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seenAccept = req.Header.Get("Accept")
		seenClientID = req.Header.Get("X-Warp-Client-ID")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Header:     make(http.Header),
		}, nil
	})

	resp, err := client.doStreamRequest(t.Context(), []byte{0x0a, 0x00}, nil)
	if err != nil {
		t.Fatalf("doStreamRequest error: %v", err)
	}
	_ = resp.Body.Close()

	if seenAccept != "text/event-stream" {
		t.Fatalf("Accept=%q want text/event-stream", seenAccept)
	}
	if seenClientID != clientID {
		t.Fatalf("X-Warp-Client-ID=%q want %q", seenClientID, clientID)
	}
}

func TestForceRefreshAccount_IgnoresSeededJWT(t *testing.T) {
	client := &Client{
		authClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != warpFirebaseURL {
					t.Fatalf("unexpected refresh url: %s", req.URL.String())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewBufferString(`{"id_token":"fresh-jwt","refresh_token":"rotated-token","expires_in":"3600"}`)),
					Header:     make(http.Header),
				}, nil
			}),
		},
		session: &session{
			refreshToken: "old-refresh",
			jwt:          "stale-seeded-jwt",
			expiresAt:    time.Now().Add(time.Hour),
			deviceID:     "device-1",
			requestID:    "request-1",
		},
	}

	jwt, err := client.ForceRefreshAccount(context.Background())
	if err != nil {
		t.Fatalf("ForceRefreshAccount() error = %v", err)
	}
	if jwt != "fresh-jwt" {
		t.Fatalf("jwt=%q want fresh-jwt", jwt)
	}
	if client.session.currentRefreshToken() != "rotated-token" {
		t.Fatalf("refresh=%q want rotated-token", client.session.currentRefreshToken())
	}
}
