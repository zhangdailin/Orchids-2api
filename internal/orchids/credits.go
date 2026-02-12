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
	orchidsActionID  = "7f0f26a524bb73a6367d10326b734efb8d9483cbf2"
	orchidsUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
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

// FetchCredits fetches the user's credits info from Orchids via RSC Server Action.
// It requires a valid Clerk __session JWT token.
func FetchCredits(ctx context.Context, sessionJWT string) (*CreditsInfo, error) {
	if strings.TrimSpace(sessionJWT) == "" {
		return nil, fmt.Errorf("empty session JWT")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", orchidsAppURL, strings.NewReader("[]"))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", orchidsUserAgent)
	req.Header.Set("Next-Action", orchidsActionID)
	req.Header.Set("Next-Router-State-Tree", `["",{"children":["__PAGE__",{},null,null]},null,null,true]`)
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
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
// It first tries SessionCookie, then falls back to fetching a fresh JWT via Clerk.
func FetchCreditsForAccount(ctx context.Context, acc interface{ GetSessionJWT() string }) (*CreditsInfo, error) {
	jwt := acc.GetSessionJWT()
	if jwt == "" {
		return nil, fmt.Errorf("no session JWT available")
	}
	return FetchCredits(ctx, jwt)
}

// PlanCreditLimit returns the monthly credit limit for a given plan name.
func PlanCreditLimit(plan string) float64 {
	if limit, ok := OrchidsPlanCredits[strings.ToUpper(plan)]; ok {
		return limit
	}
	return 150000 // Default to FREE plan
}
