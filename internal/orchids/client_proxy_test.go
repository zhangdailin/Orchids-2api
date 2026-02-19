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
