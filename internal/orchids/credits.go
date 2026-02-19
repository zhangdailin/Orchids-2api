package orchids

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	orchidsAppURL          = "https://www.orchids.app/"
	defaultOrchidsActionID = "7f876a6c1a4d682f106ecd67a0f77f2aa2d035257c"
	orchidsUserAgent       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"

	// Legacy fallback. Real value is fetched dynamically via an RSC GET.
	defaultNextRouterStateTree = `["",{"children":["__PAGE__",{},null,null]},null,null,true]`
)

// OrchidsPlanCredits maps plan names to their monthly credit limits.
var OrchidsPlanCredits = map[string]float64{
	"FREE":    150000,
	"PRO":     2000000,
	"PREMIUM": 4000000,
	"ULTRA":   12000000,
	"MAX":     30000000,
}

// CreditsInfo holds the parsed credits information from Orchids.
type CreditsInfo struct {
	Credits               float64 `json:"credits"`
	Plan                  string  `json:"plan"`
	LastFreeCreditsRedeem string  `json:"lastFreeCreditsRedeem"`
}

// jsonObjectPattern matches JSON objects in RSC response lines.
var jsonObjectPattern = regexp.MustCompile(`\{[^{}]*"credits"\s*:\s*\d+[^{}]*\}`)
var serverActionPattern = regexp.MustCompile(`createServerReference\)\("([0-9a-f]{40,})"[^)]*?"([^"]+)"\)`)

var actionCache = struct {
	mu      sync.Mutex
	id      string
	expires time.Time
}{
	id: defaultOrchidsActionID,
}

const actionCacheTTL = 6 * time.Hour

func newHTTPClientWithProxy(timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	client := &http.Client{Timeout: timeout}
	if proxyFunc != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = proxyFunc
		client.Transport = transport
	}
	return client
}

func fetchDeploymentID(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Best-effort: scrape deployment id from public JS.
	// Vercel uses format: dpl_<alnum>
	req, err := http.NewRequestWithContext(ctx, "GET", orchidsAppURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	client := newHTTPClientWithProxy(10*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deployment id html fetch failed: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	html := string(body)
	reDpl := regexp.MustCompile(`dpl_[A-Za-z0-9]+`)
	if dpl := reDpl.FindString(html); dpl != "" {
		return dpl, nil
	}
	return "", fmt.Errorf("deployment id not found")
}

func fetchNextRouterStateTree(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", orchidsAppURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("RSC", "1")
	req.Header.Set("Next-Router-Prefetch", "1")
	req.Header.Set("User-Agent", orchidsUserAgent)
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")

	client := newHTTPClientWithProxy(10*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("rsc prefetch failed: %d %s", resp.StatusCode, string(b))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// Response format: 0:{json}\n...
	lines := strings.SplitN(string(body), "\n", 2)
	if len(lines) == 0 {
		return "", fmt.Errorf("empty rsc response")
	}
	line := strings.TrimSpace(lines[0])
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", fmt.Errorf("unexpected rsc line")
	}
	payload := strings.TrimSpace(line[idx+1:])
	var root struct {
		F []any `json:"f"`
	}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		return "", err
	}
	// f is like: [ [ [tree], null, [..], true ] ]
	if len(root.F) == 0 {
		return "", fmt.Errorf("missing f")
	}
	outer, ok := root.F[0].([]any)
	if !ok || len(outer) == 0 {
		return "", fmt.Errorf("unexpected f[0]")
	}
	inner, ok := outer[0].([]any)
	if !ok || len(inner) == 0 {
		return "", fmt.Errorf("unexpected f[0][0]")
	}
	// inner[0] is the router state tree
	treeBytes, err := json.Marshal(inner[0])
	if err != nil {
		return "", err
	}
	return string(treeBytes), nil
}

func resolveOrchidsActionID(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	actionCache.mu.Lock()
	if actionCache.id != "" && time.Now().Before(actionCache.expires) {
		id := actionCache.id
		actionCache.mu.Unlock()
		return id, nil
	}
	actionCache.mu.Unlock()

	id, err := discoverOrchidsActionID(ctx, proxyFunc)
	if err != nil || strings.TrimSpace(id) == "" {
		actionCache.mu.Lock()
		actionCache.id = defaultOrchidsActionID
		actionCache.expires = time.Now().Add(30 * time.Minute)
		actionCache.mu.Unlock()
		if err != nil {
			return defaultOrchidsActionID, err
		}
		return defaultOrchidsActionID, fmt.Errorf("empty action id")
	}

	actionCache.mu.Lock()
	actionCache.id = id
	actionCache.expires = time.Now().Add(actionCacheTTL)
	actionCache.mu.Unlock()
	return id, nil
}

func discoverOrchidsActionID(ctx context.Context, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", orchidsAppURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	client := newHTTPClientWithProxy(12*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("action id html fetch failed: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	scriptURLs := extractScriptURLs(string(body))
	if len(scriptURLs) == 0 {
		return "", fmt.Errorf("no script urls found")
	}

	preferred := []string{"getUserProfile", "getUserCredits", "getCredits", "getUserUsage", "getUsage"}
	var fallbackID string

	for _, src := range scriptURLs {
		if !strings.Contains(src, "/_next/static/chunks/") {
			continue
		}
		js, err := fetchText(ctx, src, proxyFunc)
		if err != nil {
			continue
		}
		matches := serverActionPattern.FindAllStringSubmatch(js, -1)
		if len(matches) == 0 {
			continue
		}
		actions := map[string]string{}
		for _, m := range matches {
			if len(m) < 3 {
				continue
			}
			id := strings.TrimSpace(m[1])
			name := strings.TrimSpace(m[2])
			if id == "" || name == "" {
				continue
			}
			actions[name] = id
		}
		for _, name := range preferred {
			if id := actions[name]; id != "" {
				return id, nil
			}
		}
		for name, id := range actions {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "credit") || strings.Contains(lower, "usage") || strings.Contains(lower, "quota") {
				fallbackID = id
				break
			}
		}
		if fallbackID != "" {
			return fallbackID, nil
		}
	}

	return "", fmt.Errorf("no matching action id discovered")
}

func extractScriptURLs(html string) []string {
	matches := regexp.MustCompile(`src=\"([^\"]+\\.js[^\"]*)\"`).FindAllStringSubmatch(html, -1)
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
			src = strings.TrimSuffix(orchidsAppURL, "/") + src
		}
		if _, ok := seen[src]; ok {
			continue
		}
		seen[src] = struct{}{}
		out = append(out, src)
	}
	return out
}

func fetchText(ctx context.Context, url string, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	client := newHTTPClientWithProxy(12*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s failed: %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FetchCredits fetches the user's credits info from Orchids via RSC Server Action.
// It requires a valid Clerk __session JWT token and the Clerk userId.
func FetchCredits(ctx context.Context, sessionJWT string, userID string) (*CreditsInfo, error) {
	return FetchCreditsWithProxy(ctx, sessionJWT, userID, nil)
}

// FetchCreditsWithProxy fetches credits info using an optional proxy function.
func FetchCreditsWithProxy(ctx context.Context, sessionJWT string, userID string, proxyFunc func(*http.Request) (*url.URL, error)) (*CreditsInfo, error) {
	sessionJWT = strings.TrimSpace(sessionJWT)
	userID = strings.TrimSpace(userID)
	if sessionJWT == "" {
		return nil, fmt.Errorf("empty session JWT")
	}
	if userID == "" {
		return nil, fmt.Errorf("empty user id")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Server action expects an argument array. Current Orchids passes the userId.
	req, err := http.NewRequestWithContext(ctx, "POST", orchidsAppURL, strings.NewReader(fmt.Sprintf(`["%s"]`, userID)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", orchidsUserAgent)
	actionID, err := resolveOrchidsActionID(ctx, proxyFunc)
	if err != nil {
		slog.Warn("Orchids credits: action id discover failed, using fallback", "error", err)
	}
	req.Header.Set("Next-Action", actionID)

	stateTree := defaultNextRouterStateTree
	if tree, err := fetchNextRouterStateTree(ctx, proxyFunc); err == nil && strings.TrimSpace(tree) != "" {
		stateTree = tree
	}
	req.Header.Set("Next-Router-State-Tree", stateTree)

	// Orchids front-end sends an x-deployment-id header (Vercel). Without it, some
	// server actions may fail with a generic 500 digest.
	if dpl, err := fetchDeploymentID(ctx, proxyFunc); err == nil && strings.TrimSpace(dpl) != "" {
		req.Header.Set("x-deployment-id", dpl)
	}

	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	// Orchids RSC action seems to require cookies.
	req.AddCookie(&http.Cookie{Name: "__session", Value: sessionJWT})

	client := newHTTPClientWithProxy(15*time.Second, proxyFunc)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch credits: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("credits request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return parseRSCCredits(string(body))
}

// parseRSCCredits parses the RSC text/x-component response to extract credits info.
// RSC format has lines like: 0:{"credits":96571,"plan":"PRO",...}
// or the JSON may be embedded in various RSC chunk formats.
func parseRSCCredits(body string) (*CreditsInfo, error) {
	// Strategy 1: Look for JSON objects containing "credits" field
	matches := jsonObjectPattern.FindAllString(body, -1)
	for _, match := range matches {
		var info CreditsInfo
		if err := json.Unmarshal([]byte(match), &info); err == nil && info.Credits > 0 {
			return &info, nil
		}
	}

	// Strategy 2: Scan each line for embedded JSON
	lines := strings.Split(body, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// RSC lines often have format: "N:data" where N is a chunk index
		if idx := strings.Index(line, ":"); idx >= 0 && idx < 5 {
			data := line[idx+1:]
			// Try direct JSON parse
			var info CreditsInfo
			if err := json.Unmarshal([]byte(data), &info); err == nil && info.Credits > 0 {
				return &info, nil
			}
			// Try parsing as array element
			var arr []json.RawMessage
			if err := json.Unmarshal([]byte(data), &arr); err == nil {
				for _, elem := range arr {
					if err := json.Unmarshal(elem, &info); err == nil && info.Credits > 0 {
						return &info, nil
					}
				}
			}
		}

		// Strategy 3: Find "credits" anywhere in the line and extract surrounding JSON
		if strings.Contains(line, `"credits"`) {
			// Try to find JSON object boundaries
			for i := 0; i < len(line); i++ {
				if line[i] == '{' {
					depth := 0
					for j := i; j < len(line); j++ {
						if line[j] == '{' {
							depth++
						} else if line[j] == '}' {
							depth--
							if depth == 0 {
								candidate := line[i : j+1]
								var info CreditsInfo
								if err := json.Unmarshal([]byte(candidate), &info); err == nil && info.Credits > 0 {
									return &info, nil
								}
								break
							}
						}
					}
				}
			}
		}
	}

	// Log the raw body for debugging (truncated)
	preview := body
	if len(preview) > 500 {
		preview = preview[:500] + "..."
	}
	slog.Debug("Failed to parse RSC credits response", "body_preview", preview)

	return nil, fmt.Errorf("credits data not found in RSC response")
}

// PlanCreditLimit returns the monthly credit limit for a given plan name.
func PlanCreditLimit(plan string) float64 {
	if limit, ok := OrchidsPlanCredits[strings.ToUpper(plan)]; ok {
		return limit
	}
	return 150000 // Default to FREE plan
}
