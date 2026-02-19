package orchids

import (
	"net/http"
	"net/url"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestNewFromAccount_ProxyAppliedToEachAccount(t *testing.T) {
	base := &config.Config{
		ProxyHTTP: "http://proxy.local:3128",
		ProxyUser: "user",
		ProxyPass: "pass",
	}

	accounts := []*store.Account{
		{ID: 1},
		{ID: 2},
	}

	for _, acc := range accounts {
		client := NewFromAccount(acc, base)
		if client == nil || client.httpClient == nil {
			t.Fatalf("expected http client for account %d", acc.ID)
		}
		transport, ok := client.httpClient.Transport.(*http.Transport)
		if !ok || transport == nil {
			t.Fatalf("expected http.Transport for account %d", acc.ID)
		}
		if transport.Proxy == nil {
			t.Fatalf("expected proxy func for account %d", acc.ID)
		}

		proxyURL, err := transport.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}})
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

func TestNewHTTPClient_ProxyBypassAndHTTPS(t *testing.T) {
	cfg := &config.Config{
		ProxyHTTP:   "http://proxy.local:3128",
		ProxyHTTPS:  "http://secure.proxy:8443",
		ProxyUser:   "user",
		ProxyPass:   "pass",
		ProxyBypass: []string{"example.com"},
	}

	client := newHTTPClient(cfg)
	if client == nil || client.Transport == nil {
		t.Fatal("expected http client transport")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatal("expected http.Transport")
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy func")
	}

	bypassReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com"}}
	proxyURL, err := transport.Proxy(bypassReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("expected bypass to skip proxy, got %v", proxyURL)
	}

	httpsReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "orchids.app"}}
	proxyURL, err = transport.Proxy(httpsReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "secure.proxy:8443" {
		t.Fatalf("unexpected https proxy: %v", proxyURL)
	}
	if proxyURL.User == nil || proxyURL.User.Username() != "user" {
		t.Fatalf("unexpected proxy user: %v", proxyURL.User)
	}

	httpReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "orchids.app"}}
	proxyURL, err = transport.Proxy(httpReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.local:3128" {
		t.Fatalf("unexpected http proxy: %v", proxyURL)
	}
}
