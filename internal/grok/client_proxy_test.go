package grok

import (
	"net/http"
	"testing"

	"orchids-api/internal/config"
)

func TestNew_ProxyHTTPAndHTTPS(t *testing.T) {
	cfg := &config.Config{
		ProxyHTTP:  "http://proxy.local:3128",
		ProxyHTTPS: "http://secure.proxy:8443",
		ProxyUser:  "user",
		ProxyPass:  "pass",
	}

	client := New(cfg)
	if client == nil || client.httpClient == nil {
		t.Fatal("expected grok client")
	}
	transport, ok := client.httpClient.Transport.(interface {
		RoundTrip(*http.Request) (*http.Response, error)
	})
	if !ok || transport == nil {
		t.Fatal("expected transport")
	}
}

func TestNew_ProxyBypass(t *testing.T) {
	cfg := &config.Config{
		ProxyHTTP:   "http://proxy.local:3128",
		ProxyBypass: []string{"grok.com"},
	}

	client := New(cfg)
	if client == nil || client.httpClient == nil {
		t.Fatal("expected grok client")
	}
	transport, ok := client.httpClient.Transport.(interface {
		RoundTrip(*http.Request) (*http.Response, error)
	})
	if !ok || transport == nil {
		t.Fatal("expected transport")
	}
}
