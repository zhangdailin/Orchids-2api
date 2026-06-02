package util

import (
	"bufio"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// NewBrowserLikeTransport keeps the old API surface for callers that expect a
// dedicated browser-like transport with standard library connection handling.
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

type browserLikeRoundTripper struct {
	http1 *http.Transport
	http2 *http2.Transport
}

func (rt *browserLikeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && strings.EqualFold(req.URL.Scheme, "https") {
		return rt.http2.RoundTrip(req)
	}
	return rt.http1.RoundTrip(req)
}

func (rt *browserLikeRoundTripper) CloseIdleConnections() {
	if rt.http1 != nil {
		rt.http1.CloseIdleConnections()
	}
	if rt.http2 != nil {
		rt.http2.CloseIdleConnections()
	}
}

var browserHTTPClientCache struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
}

func init() {
	browserHTTPClientCache.clients = make(map[string]*http.Client)
}

func GetSharedBrowserHTTPClient(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	if proxyKey == "" {
		proxyKey = "direct"
	}
	cacheKey := "browser|" + sharedHTTPClientCacheKey(proxyKey, timeout)

	browserHTTPClientCache.mu.RLock()
	client, ok := browserHTTPClientCache.clients[cacheKey]
	browserHTTPClientCache.mu.RUnlock()
	if ok {
		return client
	}

	browserHTTPClientCache.mu.Lock()
	defer browserHTTPClientCache.mu.Unlock()
	if client, ok = browserHTTPClientCache.clients[cacheKey]; ok {
		return client
	}

	rt := &browserLikeRoundTripper{
		http1: &http.Transport{
			Proxy:                 proxyFunc,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			MaxConnsPerHost:       200,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeoutForClient(timeout),
			TLSClientConfig:       &stdtls.Config{MinVersion: stdtls.VersionTLS12},
		},
		http2: &http2.Transport{
			AllowHTTP: false,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *stdtls.Config) (net.Conn, error) {
				return dialUTLSHTTP2Context(ctx, network, addr, cfg, proxyFunc)
			},
			TLSClientConfig: &stdtls.Config{
				MinVersion: stdtls.VersionTLS12,
				NextProtos: []string{"h2"},
			},
		},
	}

	client = &http.Client{
		Transport: rt,
		Timeout:   timeout,
	}
	browserHTTPClientCache.clients[cacheKey] = client
	return client
}

func dialUTLSHTTP2Context(ctx context.Context, network, addr string, cfg *stdtls.Config, proxyFunc func(*http.Request) (*url.URL, error)) (net.Conn, error) {
	rawConn, targetHost, err := dialHTTPSProxyAware(ctx, network, addr, proxyFunc)
	if err != nil {
		return nil, err
	}

	serverName := targetHost
	if cfg != nil && strings.TrimSpace(cfg.ServerName) != "" {
		serverName = strings.TrimSpace(cfg.ServerName)
	}
	utlsCfg := &utls.Config{
		ServerName: serverName,
		MinVersion: utls.VersionTLS12,
		NextProtos: []string{"h2"},
	}
	conn := utls.UClient(rawConn, utlsCfg, utls.HelloChrome_Auto)
	if err := conn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}
	if proto := conn.ConnectionState().NegotiatedProtocol; proto != http2.NextProtoTLS {
		conn.Close()
		return nil, fmt.Errorf("browser http2: unexpected ALPN protocol %q", proto)
	}
	return conn, nil
}

func dialHTTPSProxyAware(ctx context.Context, network, addr string, proxyFunc func(*http.Request) (*url.URL, error)) (net.Conn, string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", err
	}

	if proxyFunc == nil {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		return conn, host, err
	}

	req := &http.Request{URL: &url.URL{Scheme: "https", Host: addr}}
	proxyURL, err := proxyFunc(req)
	if err != nil {
		return nil, "", err
	}
	if proxyURL == nil {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
		return conn, host, err
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "" {
		return nil, "", fmt.Errorf("browser http2 proxy scheme %q is not supported", proxyURL.Scheme)
	}

	proxyAddr := proxyURL.Host
	if _, _, splitErr := net.SplitHostPort(proxyAddr); splitErr != nil {
		proxyAddr = net.JoinHostPort(proxyAddr, "80")
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, proxyAddr)
	if err != nil {
		return nil, "", err
	}
	if err := writeHTTPConnect(ctx, conn, addr, proxyURL); err != nil {
		conn.Close()
		return nil, "", err
	}
	return conn, host, nil
}

func writeHTTPConnect(ctx context.Context, conn net.Conn, target string, proxyURL *url.URL) error {
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if proxyURL != nil && proxyURL.User != nil {
		user := proxyURL.User.Username()
		password := ""
		if pass, ok := proxyURL.User.Password(); ok {
			password = pass
		}
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := connectReq.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), connectReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy CONNECT failed: %s", resp.Status)
	}
	return nil
}
