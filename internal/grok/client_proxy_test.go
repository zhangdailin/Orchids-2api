package grok

import (
	"net/http"
	"net/url"
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
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatal("expected http.Transport")
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy func")
	}

	httpsReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "grok.com"}}
	proxyURL, err := transport.Proxy(httpsReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "secure.proxy:8443" {
		t.Fatalf("unexpected https proxy: %v", proxyURL)
	}
	if proxyURL.User == nil || proxyURL.User.Username() != "user" {
		t.Fatalf("unexpected proxy user: %v", proxyURL.User)
	}
	if pass, ok := proxyURL.User.Password(); !ok || pass != "pass" {
		t.Fatalf("unexpected proxy password")
	}

	httpReq := &http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}}
	proxyURL, err = transport.Proxy(httpReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.local:3128" {
		t.Fatalf("unexpected http proxy: %v", proxyURL)
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
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatal("expected http.Transport")
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy func")
	}

	bypassReq := &http.Request{URL: &url.URL{Scheme: "https", Host: "grok.com"}}
	proxyURL, err := transport.Proxy(bypassReq)
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("expected bypass to skip proxy, got %v", proxyURL)
	}
}
