package util

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"orchids-api/internal/config"
)

func ProxyFunc(httpProxy, httpsProxy, user, pass string, bypass []string) func(*http.Request) (*url.URL, error) {
	httpProxy = strings.TrimSpace(httpProxy)
	httpsProxy = strings.TrimSpace(httpsProxy)
	user = strings.TrimSpace(user)
	pass = strings.TrimSpace(pass)

	parseProxy := func(raw string) *url.URL {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil
		}
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil
		}
		if u.Host == "" {
			return nil
		}
		if user != "" && u.User == nil {
			u.User = url.UserPassword(user, pass)
		}
		return u
	}

	httpURL := parseProxy(httpProxy)
	httpsURL := parseProxy(httpsProxy)
	useEnv := httpURL == nil && httpsURL == nil

	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		if shouldBypass(req.URL.Host, bypass) {
			return nil, nil
		}

		scheme := strings.ToLower(strings.TrimSpace(req.URL.Scheme))
		isSecure := scheme == "https" || scheme == "wss"
		if isSecure {
			if httpsURL != nil {
				return httpsURL, nil
			}
			if httpURL != nil {
				return httpURL, nil
			}
		} else {
			if httpURL != nil {
				return httpURL, nil
			}
			if httpsURL != nil {
				return httpsURL, nil
			}
		}

		if useEnv {
			return http.ProxyFromEnvironment(req)
		}
		return nil, nil
	}
}

func DirectProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) {
		return nil, nil
	}
}

func ProxyFuncFromURL(proxyURL *url.URL, bypass []string) func(*http.Request) (*url.URL, error) {
	if proxyURL == nil {
		return DirectProxyFunc()
	}
	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		if shouldBypass(req.URL.Host, bypass) {
			return nil, nil
		}
		return proxyURL, nil
	}
}

func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, nil
	}
}

func ProxyURLFromConfig(cfg *config.Config) *url.URL {
	if cfg == nil {
		return nil
	}
	if u, err := ParseProxyURL(cfg.ProxyURL); err == nil && u != nil {
		return u
	}

	httpProxy := strings.TrimSpace(cfg.ProxyHTTP)
	httpsProxy := strings.TrimSpace(cfg.ProxyHTTPS)
	user := strings.TrimSpace(cfg.ProxyUser)
	pass := strings.TrimSpace(cfg.ProxyPass)

	target := httpProxy
	if target == "" {
		target = httpsProxy
	}
	u, err := ParseProxyURL(target)
	if err != nil || u == nil {
		return nil
	}
	if user != "" && u.User == nil {
		u.User = url.UserPassword(user, pass)
	}
	return u
}

func ProxyFuncFromConfig(cfg *config.Config) func(*http.Request) (*url.URL, error) {
	if cfg == nil {
		return http.ProxyFromEnvironment
	}
	if proxyURL, err := ParseProxyURL(cfg.ProxyURL); err == nil && proxyURL != nil {
		if user := strings.TrimSpace(cfg.ProxyUser); user != "" && proxyURL.User == nil {
			proxyURL.User = url.UserPassword(user, cfg.ProxyPass)
		}
		return ProxyFuncFromURL(proxyURL, cfg.ProxyBypass)
	}
	if strings.TrimSpace(cfg.ProxyHTTP) != "" || strings.TrimSpace(cfg.ProxyHTTPS) != "" {
		return ProxyFunc(cfg.ProxyHTTP, cfg.ProxyHTTPS, cfg.ProxyUser, cfg.ProxyPass, cfg.ProxyBypass)
	}
	return DirectProxyFunc()
}

func shouldBypass(host string, bypass []string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	hostIP := net.ParseIP(host)

	for _, raw := range bypass {
		entry := normalizeHost(raw)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		entry = strings.TrimPrefix(entry, "*.")
		if strings.Contains(entry, "/") {
			if hostIP == nil {
				continue
			}
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(hostIP) {
				return true
			}
			continue
		}
		if hostIP != nil {
			if ip := net.ParseIP(entry); ip != nil && ip.Equal(hostIP) {
				return true
			}
		}
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			raw = u.Host
		}
	}
	if strings.Contains(raw, ":") {
		if host, _, err := net.SplitHostPort(raw); err == nil {
			raw = host
		} else {
			raw = strings.Trim(raw, "[]")
		}
	}
	raw = strings.TrimLeft(raw, ".")
	return strings.ToLower(strings.TrimSpace(raw))
}
