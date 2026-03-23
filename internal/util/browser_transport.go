package util

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"
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
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}
