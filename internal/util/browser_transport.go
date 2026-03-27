package util

import (
	"context"
	stdtls "crypto/tls"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// NewBrowserLikeTransport keeps the old API surface for callers that expect a
// dedicated browser-like transport, but it now uses the standard library
// transport only. This removes the custom TLS stack while preserving sane
// pooling, proxy, and timeout behavior.
func NewBrowserLikeTransport(proxyFunc func(*http.Request) (*url.URL, error)) http.RoundTripper {
	return &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &stdtls.Config{MinVersion: stdtls.VersionTLS12},
	}
}

var utlsClientCache struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
}

func init() {
	utlsClientCache.clients = make(map[string]*http.Client)
}

func GetSharedUTLSHTTPClient(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	if proxyKey == "" {
		proxyKey = "direct"
	}
	cacheKey := "utls|" + proxyKey

	utlsClientCache.mu.RLock()
	client, ok := utlsClientCache.clients[cacheKey]
	utlsClientCache.mu.RUnlock()
	if ok {
		if client.Timeout != timeout {
			clone := *client
			clone.Timeout = timeout
			return &clone
		}
		return client
	}

	utlsClientCache.mu.Lock()
	defer utlsClientCache.mu.Unlock()

	if client, ok = utlsClientCache.clients[cacheKey]; ok {
		if client.Timeout != timeout {
			clone := *client
			clone.Timeout = timeout
			return &clone
		}
		return client
	}

	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DialTLSContext:        dialUTLSContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       200,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	newClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	utlsClientCache.clients[cacheKey] = newClient
	return newClient
}

func dialUTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	rawConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		rawConn.Close()
		return nil, err
	}

	config := &utls.Config{
		ServerName: host,
		MinVersion: utls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
	}
	conn := utls.UClient(rawConn, config, utls.HelloChrome_Auto)
	if err := conn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	return conn, nil
}
