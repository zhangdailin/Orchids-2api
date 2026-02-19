package orchids

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

type PublicModelChoice struct {
	ID   string
	Name string
}

var publicModelPattern = regexp.MustCompile(`value:"([^"]+)"[^}]*?label:"([^"]+)"[^}]*?supportsOrchids:!0`)

var fallbackPublicModels = []PublicModelChoice{
	{ID: "auto", Name: "Auto"},
	{ID: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	{ID: "claude-opus-4.6", Name: "Claude Opus 4.6"},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5"},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash"},
	{ID: "gemini-3-pro", Name: "Gemini 3 Pro"},
	{ID: "gpt-5.2-codex", Name: "GPT-5.2 Codex"},
	{ID: "gpt-5.2", Name: "GPT-5.2"},
	{ID: "grok-4.1-fast", Name: "Grok 4.1 Fast"},
	{ID: "glm-5", Name: "GLM 5"},
	{ID: "kimi-k2.5", Name: "Kimi K2.5"},
}

var publicModelCache = struct {
	mu      sync.Mutex
	items   []PublicModelChoice
	expires time.Time
}{
	items: nil,
}

const publicModelCacheTTL = 30 * time.Minute

func FetchPublicModelChoices(ctx context.Context) ([]PublicModelChoice, error) {
	return FetchPublicModelChoicesWithProxy(ctx, nil)
}

func FetchPublicModelChoicesWithProxy(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) ([]PublicModelChoice, error) {
	publicModelCache.mu.Lock()
	if time.Now().Before(publicModelCache.expires) && len(publicModelCache.items) > 0 {
		items := make([]PublicModelChoice, len(publicModelCache.items))
		copy(items, publicModelCache.items)
		publicModelCache.mu.Unlock()
		return items, nil
	}
	publicModelCache.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", orchidsAppURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	client := newHTTPClientWithProxy(12*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fallbackPublicModels, fmt.Errorf("public models html fetch failed: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fallbackPublicModels, err
	}

	scriptURLs := extractScriptURLs(string(body))
	if len(scriptURLs) == 0 {
		return fallbackPublicModels, fmt.Errorf("no script urls found")
	}

	seen := map[string]struct{}{}
	out := make([]PublicModelChoice, 0, 16)
	for _, src := range scriptURLs {
		if !strings.Contains(src, "/_next/static/chunks/") {
			continue
		}
		js, err := fetchText(ctx, src, proxyFunc)
		if err != nil {
			continue
		}
		if !strings.Contains(js, "supportsOrchids") {
			continue
		}
		matches := publicModelPattern.FindAllStringSubmatch(js, -1)
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
			out = append(out, PublicModelChoice{ID: id, Name: name})
		}
		if len(out) > 0 {
			break
		}
	}

	if len(out) == 0 {
		return fallbackPublicModels, fmt.Errorf("no public model choices found")
	}

	publicModelCache.mu.Lock()
	publicModelCache.items = out
	publicModelCache.expires = time.Now().Add(publicModelCacheTTL)
	publicModelCache.mu.Unlock()

	return out, nil
}
