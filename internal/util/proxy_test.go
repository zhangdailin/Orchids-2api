package util

import (
	"net/http"
	"net/url"
	"testing"
)

func TestProxyFunc_NoSchemeDefaultsToHTTP(t *testing.T) {
	proxyFunc := ProxyFunc("proxy.local:3128", "", "", "", nil)
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "http", Host: "example.com"}})
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected proxy url")
	}
	if proxyURL.Scheme != "http" {
		t.Fatalf("expected http scheme, got %q", proxyURL.Scheme)
	}
	if proxyURL.Host != "proxy.local:3128" {
		t.Fatalf("unexpected proxy host: %s", proxyURL.Host)
	}
}

func TestProxyFunc_WSSUsesHTTPSProxy(t *testing.T) {
	proxyFunc := ProxyFunc("http://proxy.local:3128", "http://secure.proxy:8443", "", "", nil)
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "wss", Host: "example.com"}})
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "secure.proxy:8443" {
		t.Fatalf("unexpected proxy url: %v", proxyURL)
	}
}

func TestProxyFunc_LeadingDotBypass(t *testing.T) {
	proxyFunc := ProxyFunc("http://proxy.local:3128", "", "", "", []string{".example.com"})
	proxyURL, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com"}})
	if err != nil {
		t.Fatalf("proxy func failed: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("expected bypass, got %v", proxyURL)
	}
}
