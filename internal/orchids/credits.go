package orchids

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	orchidsAppURL    = "https://www.orchids.app/"
	orchidsActionID  = "7f876a6c1a4d682f106ecd67a0f77f2aa2d035257c"
	orchidsUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"

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

func fetchDeploymentID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Best-effort: scrape deployment id from public JS.
	// Vercel uses format: dpl_<alnum>
	req, err := http.NewRequestWithContext(ctx, "GET", orchidsAppURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	client := &http.Client{Timeout: 10 * time.Second}
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

func fetchNextRouterStateTree(ctx context.Context) (string, error) {
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

	client := &http.Client{Timeout: 10 * time.Second}
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

// FetchCredits fetches the user's credits info from Orchids via RSC Server Action.
// It requires a valid Clerk __session JWT token and the Clerk userId.
func FetchCredits(ctx context.Context, sessionJWT string, userID string) (*CreditsInfo, error) {
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
	req.Header.Set("Next-Action", orchidsActionID)

	stateTree := defaultNextRouterStateTree
	if tree, err := fetchNextRouterStateTree(ctx); err == nil && strings.TrimSpace(tree) != "" {
		stateTree = tree
	}
	req.Header.Set("Next-Router-State-Tree", stateTree)

	// Orchids front-end sends an x-deployment-id header (Vercel). Without it, some
	// server actions may fail with a generic 500 digest.
	if dpl, err := fetchDeploymentID(ctx); err == nil && strings.TrimSpace(dpl) != "" {
		req.Header.Set("x-deployment-id", dpl)
	}

	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	// Orchids RSC action seems to require cookies.
	req.AddCookie(&http.Cookie{Name: "__session", Value: sessionJWT})

	client := &http.Client{Timeout: 15 * time.Second}
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

// FetchCreditsForAccount fetches credits for an Orchids account using its stored credentials.
func FetchCreditsForAccount(ctx context.Context, acc interface{ GetSessionJWT() string; GetUserID() string }) (*CreditsInfo, error) {
	jwt := strings.TrimSpace(acc.GetSessionJWT())
	uid := strings.TrimSpace(acc.GetUserID())
	if jwt == "" {
		return nil, fmt.Errorf("no session JWT available")
	}
	if uid == "" {
		return nil, fmt.Errorf("no user id available")
	}
	return FetchCredits(ctx, jwt, uid)
}

// PlanCreditLimit returns the monthly credit limit for a given plan name.
func PlanCreditLimit(plan string) float64 {
	if limit, ok := OrchidsPlanCredits[strings.ToUpper(plan)]; ok {
		return limit
	}
	return 150000 // Default to FREE plan
}
