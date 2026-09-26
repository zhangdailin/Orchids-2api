package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
)

func TestGrokClientsUseConfiguredProxy(t *testing.T) {
	// A local forwarder is enough to assert the outgoing transport's route;
	// no real upstream credentials or network access are needed.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer proxy.Close()
	target := "http://upstream.invalid/device"
	cfg := &config.Config{ProxyURL: proxy.URL}

	cli := NewCLIClient(cfg)
	if cli.httpClient == nil {
		t.Fatal("missing Grok CLI client")
	}
	for _, tc := range []struct {
		name   string
		client *http.Client
	}{
		{name: "cli", client: cli.httpClient},
		{name: "device", client: NewDeviceAuthenticator(cfg).httpClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tc.client.Do(req)
			if err != nil {
				t.Fatalf("client did not reach configured proxy: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusTeapot {
				t.Fatalf("status = %d, want proxy's status", resp.StatusCode)
			}
		})
	}

	// Proxy changes must allocate a new shared browser transport.
	other := &config.Config{ProxyURL: "http://different.proxy.invalid:3128"}
	if cli.httpClient == NewCLIClient(other).httpClient {
		t.Fatal("Grok CLI reused a cached client after proxy change")
	}
	if NewDeviceAuthenticator(cfg).httpClient == NewDeviceAuthenticator(other).httpClient {
		t.Fatal("device auth reused a cached client after proxy change")
	}
}
