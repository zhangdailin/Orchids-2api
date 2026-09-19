package util

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
)

// HttpClientCache provides a thread-safe cache for http.Client instances
// based on their proxy configuration. This ensures that we reuse TCP connections
// (Keep-Alive) instead of exhausting ephemeral ports and paying the TLS handshake
// penalty on every upstream request.
type clientPool struct {
	mu      sync.RWMutex
	clients map[string]*http.Client
}

const maxSharedHTTPClients = 512

// evictOneClientLocked bounds process-wide connection pools. Callers hold p.mu.
// A deterministic LRU is unnecessary here because keys are configuration
// identities; bounding and closing any stale pool prevents unbounded growth.
func (p *clientPool) evictOneClientLocked() {
	if p == nil || len(p.clients) < maxSharedHTTPClients {
		return
	}
	for key, client := range p.clients {
		delete(p.clients, key)
		if client != nil {
			client.CloseIdleConnections()
		}
		return
	}
}

var httpClientCache = clientPool{clients: make(map[string]*http.Client)}

// GetSharedHTTPClient returns a shared http.Client.
// The proxyKey should uniquely identify the proxy configuration (e.g., the Proxy URL or "direct").
// Transport configuration (like timeouts) should be uniform per proxyKey.
func GetSharedHTTPClient(proxyKey string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	if proxyKey == "" {
		proxyKey = "direct"
	}
	cacheKey := sharedHTTPClientCacheKey(proxyKey, timeout)

	httpClientCache.mu.RLock()
	client, ok := httpClientCache.clients[cacheKey]
	httpClientCache.mu.RUnlock()
	if ok {
		return client
	}

	httpClientCache.mu.Lock()
	defer httpClientCache.mu.Unlock()

	// Double check
	if client, ok = httpClientCache.clients[cacheKey]; ok {
		return client
	}

	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       200, // Important for High concurrency
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeoutForClient(timeout),
		Proxy:                 proxyFunc,
		TLSClientConfig:       &tls.Config{},
	}

	newClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	httpClientCache.evictOneClientLocked()
	httpClientCache.clients[cacheKey] = newClient
	return newClient
}

func sharedHTTPClientCacheKey(proxyKey string, timeout time.Duration) string {
	return fmt.Sprintf("%s|timeout=%d", proxyKey, int64(timeout/time.Second))
}

func responseHeaderTimeoutForClient(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 30 * time.Second
	}
	if timeout <= 30*time.Second {
		return timeout
	}
	return min(max(timeout/2, 60*time.Second), 120*time.Second)
}

// generateProxyKey generates a string key based on the proxy config.
func GenerateProxyKey(proxyHTTP, proxyHTTPS, proxyUser string) string {
	if proxyHTTP == "" && proxyHTTPS == "" {
		return "direct"
	}
	// Combine to strictly separate different proxy configurations
	return proxyHTTP + "|" + proxyHTTPS + "|" + proxyUser
}

func GenerateProxyKeyFromConfig(cfg *config.Config) string {
	if cfg == nil {
		return "env"
	}
	if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
		key := proxyURL
		if len(cfg.ProxyBypass) > 0 {
			key += "|" + strings.Join(cfg.ProxyBypass, ",")
		}
		return key
	}
	key := GenerateProxyKey(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser)
	if len(cfg.ProxyBypass) > 0 {
		key += "|" + strings.Join(cfg.ProxyBypass, ",")
	}
	return key
}
