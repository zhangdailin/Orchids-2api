package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type orchidsPublicModelChoice struct {
	ID   string
	Name string
}

var orchidsPublicModelPattern = regexp.MustCompile(`value:"([^"]+)"[^}]*?label:"([^"]+)"[^}]*?supportsOrchids\s*:\s*(?:!0|true)`)

var orchidsFallbackPublicModels = []orchidsPublicModelChoice{
	{ID: "auto", Name: "Auto"},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	{ID: "claude-opus-4.6", Name: "Claude Opus 4.6"},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
	{ID: "gemini-3.1-pro", Name: "Gemini 3.1 Pro"},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash"},
	{ID: "gemini-3-pro", Name: "Gemini 3 Pro"},
	{ID: "gpt-5.3-codex", Name: "GPT-5.3 Codex"},
	{ID: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
	{ID: "gpt-5.2", Name: "GPT-5.2"},
	{ID: "grok-4.1-fast", Name: "Grok 4.1 Fast"},
	{ID: "glm-5", Name: "GLM 5"},
	{ID: "kimi-k2.5", Name: "Kimi K2.5"},
}

var orchidsPublicModelCache = struct {
	mu      sync.Mutex
	items   []orchidsPublicModelChoice
	expires time.Time
}{
	items: nil,
}

const (
	orchidsPublicModelCacheTTL = 30 * time.Minute
	orchidsPublicAppURL        = "https://www.orchids.app/"
	orchidsPublicUserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
)

func fetchOrchidsPublicModelChoicesWithProxy(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) ([]orchidsPublicModelChoice, error) {
	orchidsPublicModelCache.mu.Lock()
	if time.Now().Before(orchidsPublicModelCache.expires) && len(orchidsPublicModelCache.items) > 0 {
		items := make([]orchidsPublicModelChoice, len(orchidsPublicModelCache.items))
		copy(items, orchidsPublicModelCache.items)
		orchidsPublicModelCache.mu.Unlock()
		return items, nil
	}
	orchidsPublicModelCache.mu.Unlock()

	htmlCtx, cancelHTML := context.WithTimeout(ctx, 12*time.Second)
	defer cancelHTML()

	req, err := http.NewRequestWithContext(htmlCtx, "GET", orchidsPublicAppURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", orchidsPublicUserAgent)
	client := orchidsHTTPClientWithProxy(12*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return orchidsFallbackPublicModels, fmt.Errorf("public models html fetch failed: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return orchidsFallbackPublicModels, err
	}

	scriptURLs := extractOrchidsScriptURLs(string(body))
	if len(scriptURLs) == 0 {
		return orchidsFallbackPublicModels, fmt.Errorf("no script urls found")
	}

	scriptCtx, cancelScripts := context.WithTimeout(ctx, 25*time.Second)
	defer cancelScripts()

	seen := map[string]struct{}{}
	out := make([]orchidsPublicModelChoice, 0, 16)
	for _, src := range scriptURLs {
		if !strings.Contains(src, "/_next/static/chunks/") {
			continue
		}
		js, err := fetchOrchidsText(scriptCtx, src, proxyFunc)
		if err != nil {
			continue
		}
		matches := orchidsPublicModelPattern.FindAllStringSubmatch(js, -1)
		for _, m := range matches {
			if len(m) < 3 {
				continue
			}
			id := strings.TrimSpace(m[1])
			name := strings.TrimSpace(m[2])
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, orchidsPublicModelChoice{ID: id, Name: name})
		}
		if len(out) > 0 {
			break
		}
	}

	if len(out) == 0 {
		return orchidsFallbackPublicModels, fmt.Errorf("no public model choices found")
	}

	orchidsPublicModelCache.mu.Lock()
	orchidsPublicModelCache.items = out
	orchidsPublicModelCache.expires = time.Now().Add(orchidsPublicModelCacheTTL)
	orchidsPublicModelCache.mu.Unlock()

	return out, nil
}

func orchidsHTTPClientWithProxy(timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	if proxyFunc == nil {
		proxyFunc = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: proxyFunc,
		},
	}
}

func extractOrchidsScriptURLs(html string) []string {
	re := regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	matches := re.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		if src == "" {
			continue
		}
		if strings.HasPrefix(src, "/") {
			src = strings.TrimSuffix(orchidsPublicAppURL, "/") + src
		}
		if _, ok := seen[src]; ok {
			continue
		}
		seen[src] = struct{}{}
		out = append(out, src)
	}
	return out
}

func fetchOrchidsText(ctx context.Context, targetURL string, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsPublicUserAgent)
	client := orchidsHTTPClientWithProxy(12*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
