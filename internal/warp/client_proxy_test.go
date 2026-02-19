package warp

import (
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
		{ID: 1, RefreshToken: "token-1"},
		{ID: 2, RefreshToken: "token-2"},
	}

	for _, acc := range accounts {
		client := NewFromAccount(acc, base)
		if client == nil || client.httpClient == nil {
			t.Fatalf("expected http client for account %d", acc.ID)
		}
		transport, ok := client.httpClient.Transport.(*utlsTransport)
		if !ok || transport == nil {
			t.Fatalf("expected utls transport for account %d", acc.ID)
		}
		if transport.proxyURL == nil {
			t.Fatalf("expected proxy url for account %d", acc.ID)
		}
		if transport.proxyURL.Host != "proxy.local:3128" {
			t.Fatalf("unexpected proxy host for account %d: %s", acc.ID, transport.proxyURL.Host)
		}
		if transport.proxyURL.User == nil || transport.proxyURL.User.Username() != "user" {
			t.Fatalf("unexpected proxy user for account %d", acc.ID)
		}
		if pass, ok := transport.proxyURL.User.Password(); !ok || pass != "pass" {
			t.Fatalf("unexpected proxy password for account %d", acc.ID)
		}
	}
}
