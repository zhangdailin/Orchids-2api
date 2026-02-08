package orchids

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/clerk"

	"golang.org/x/sync/singleflight"
)

const (
	orchidsAppURL = "https://www.orchids.app/"
	// orchidsActionID 是一个兜底的 Server Action ID。
	// 注意：Orchids 前端每次部署都可能变更该 ID，所以 FetchCredits 会优先尝试动态解析。
	orchidsActionID  = "7f876a6c1a4d682f106ecd67a0f77f2aa2d035257c"
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

// CreditsAuth 用于请求 Orchids credits 所需的登录态信息。
// - SessionJWT: 对应 __session
// - ClientJWT:  对应 __client
// - ClientUat:  对应 __client_uat
type CreditsAuth struct {
	SessionJWT string
	ClientJWT  string
	ClientUat  string
	SessionID  string
	UserID     string
}

// jsonObjectPattern matches JSON objects in RSC response lines.
var jsonObjectPattern = regexp.MustCompile(`\{[^{}]*"credits"\s*:\s*\d+(?:\.\d+)?[^{}]*\}`)

var (
	creditsActionIDMu      sync.Mutex
	cachedCreditsActionID  string
	cachedCreditsActionExp time.Time
	creditsActionIDSF      singleflight.Group
)

var (
	scriptSrcPattern       = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	serverActionRefPattern = regexp.MustCompile(`createServerReference\)\(\"([0-9a-f]{40,64})\"[^\)]*\"(getUserProfile|fetchUserProfile|getCredits|getUserCredits)\"\)`)
)

type creditsRequestError struct {
	ActionID    string
	StatusCode  int
	BodyPreview string
	AuthStatus  string
	AuthReason  string
	AuthMessage string
	VercelError string
	ContentType string
}

func (e *creditsRequestError) Error() string {
	return fmt.Sprintf("credits request failed with status %d", e.StatusCode)
}

type creditsSoftError struct {
	ActionID string
	Reason   string
}

func (e *creditsSoftError) Error() string {
	if e == nil {
		return "credits soft error"
	}
	if e.ActionID == "" {
		return fmt.Sprintf("credits soft error: %s", e.Reason)
	}
	return fmt.Sprintf("credits soft error: %s (action_id=%s)", e.Reason, e.ActionID)
}

// IsCreditsSoftError reports whether err is a non-fatal credits sync error.
func IsCreditsSoftError(err error) bool {
	var softErr *creditsSoftError
	return errors.As(err, &softErr)
}

func isKnownDigest2110345482(reqErr *creditsRequestError) bool {
	if reqErr == nil {
		return false
	}
	return reqErr.StatusCode == http.StatusInternalServerError && strings.Contains(reqErr.BodyPreview, `"digest":"2110345482"`)
}

// FetchCredits fetches the user's credits info from Orchids via RSC Server Action.
// It requires a valid Clerk __session JWT token.
func FetchCredits(ctx context.Context, sessionJWT string) (*CreditsInfo, error) {
	return FetchCreditsWithAuth(ctx, CreditsAuth{SessionJWT: sessionJWT})
}

// FetchCreditsWithAuth fetches credits with full Clerk cookies when available.
func FetchCreditsWithAuth(ctx context.Context, auth CreditsAuth) (*CreditsInfo, error) {
	auth.SessionJWT = strings.TrimSpace(auth.SessionJWT)
	auth.ClientJWT = strings.TrimSpace(auth.ClientJWT)
	auth.ClientUat = strings.TrimSpace(auth.ClientUat)
	auth.SessionID = strings.TrimSpace(auth.SessionID)
	auth.UserID = strings.TrimSpace(auth.UserID)

	if auth.SessionJWT == "" {
		return nil, fmt.Errorf("empty session JWT")
	}
	if auth.UserID == "" {
		_, sub := clerk.ParseSessionInfoFromJWT(auth.SessionJWT)
		auth.UserID = strings.TrimSpace(sub)
	}
	if auth.UserID == "" {
		return nil, fmt.Errorf("missing userID for credits action")
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// credits 的 Server Action 依赖 Clerk session cookie（__session）。该 JWT 通常较短期，
	// 若过期会直接导致 500（x_clerk_auth_reason=token-expired / session-token-expired...）。
	// 这里在请求前先做一次「到期预检」，若已过期/即将过期则用 __client + sessionID 走 Clerk token endpoint 续期。
	if auth.ClientJWT != "" {
		now := time.Now().Unix()
		exp := parseJWTExp(auth.SessionJWT)
		if exp > 0 && exp <= now+30 {
			sid := auth.SessionID
			if sid == "" {
				sid, _ = clerk.ParseSessionInfoFromJWT(auth.SessionJWT)
			}
			if sid != "" {
				refreshed, err := refreshClerkSessionJWT(ctx, sid, auth.ClientJWT, auth.ClientUat)
				if err != nil {
					slog.Warn("Orchids credits: session JWT 刷新失败", "session_id", sid, "error", err)
				} else if refreshed != "" {
					slog.Debug("Orchids credits: session JWT 已过期/即将过期，已刷新", "old_exp", exp, "now", now)
					auth.SessionJWT = refreshed
				}
			}
		}
	}

	actionID, actionErr := getCreditsActionID(ctx, false)
	if actionErr != nil {
		// 解析失败不应阻断主流程，直接用兜底 ID 尝试。
		slog.Debug("Orchids credits: 动态解析 actionID 失败，使用兜底值", "error", actionErr)
		actionID = orchidsActionID
	}

	info, err := fetchCreditsOnce(ctx, auth, actionID)
	if err == nil {
		return info, nil
	}

	var reqErr *creditsRequestError
	if errors.As(err, &reqErr) {
		if isKnownDigest2110345482(reqErr) {
			slog.Debug("Orchids credits: 命中已知 digest，跳过更新并保持原额度", "action_id", reqErr.ActionID)
			return nil, &creditsSoftError{ActionID: reqErr.ActionID, Reason: "known-digest-2110345482"}
		}
		slog.Debug("Orchids credits 请求失败",
			"action_id", reqErr.ActionID,
			"status_code", reqErr.StatusCode,
			"x_clerk_auth_status", reqErr.AuthStatus,
			"x_clerk_auth_reason", reqErr.AuthReason,
			"x_clerk_auth_message", reqErr.AuthMessage,
			"x_vercel_error", reqErr.VercelError,
			"content_type", reqErr.ContentType,
			"body_preview", reqErr.BodyPreview,
		)
	} else {
		slog.Debug("Orchids credits 请求失败", "action_id", actionID, "error", err)
	}

	// 兼容：Orchids 可能已部署更新，导致 actionID 变更，旧 ID 会直接返回 500。
	// 这里强制刷新一次 actionID 并重试。
	refreshedActionID, refreshErr := getCreditsActionID(ctx, true)
	if refreshErr != nil {
		return nil, err
	}
	if refreshedActionID == "" || refreshedActionID == actionID {
		return nil, err
	}

	statusCode := 0
	if reqErr != nil {
		statusCode = reqErr.StatusCode
	}
	slog.Debug("Orchids credits: actionID 可能已变更，准备重试", "old_action_id", actionID, "new_action_id", refreshedActionID, "status_code", statusCode)

	info, retryErr := fetchCreditsOnce(ctx, auth, refreshedActionID)
	if retryErr != nil {
		if errors.As(retryErr, &reqErr) {
			if isKnownDigest2110345482(reqErr) {
				slog.Debug("Orchids credits: 重试命中已知 digest，跳过更新并保持原额度", "action_id", reqErr.ActionID)
				return nil, &creditsSoftError{ActionID: reqErr.ActionID, Reason: "known-digest-2110345482"}
			}
			slog.Debug("Orchids credits 重试失败",
				"action_id", reqErr.ActionID,
				"status_code", reqErr.StatusCode,
				"x_clerk_auth_status", reqErr.AuthStatus,
				"x_clerk_auth_reason", reqErr.AuthReason,
				"x_clerk_auth_message", reqErr.AuthMessage,
				"x_vercel_error", reqErr.VercelError,
				"content_type", reqErr.ContentType,
				"body_preview", reqErr.BodyPreview,
			)
		} else {
			slog.Debug("Orchids credits 重试失败", "action_id", refreshedActionID, "error", retryErr)
		}
		return nil, retryErr
	}
	return info, nil
}

func fetchCreditsOnce(ctx context.Context, auth CreditsAuth, actionID string) (*CreditsInfo, error) {
	payload, err := json.Marshal([]string{auth.UserID})
	if err != nil {
		return nil, fmt.Errorf("failed to encode credits action payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", orchidsAppURL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", orchidsUserAgent)
	req.Header.Set("Next-Action", actionID)
	req.Header.Set("Next-Router-State-Tree", `["",{"children":["__PAGE__",{},null,null]},null,null,true]`)
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	req.AddCookie(&http.Cookie{Name: "__session", Value: auth.SessionJWT})

	// Clerk 中间件对 __client_uat 和 session token 的 iat 有约束：
	// 若 __client_uat 比 session token 的 iat 更新，会被判定为 signed-out
	// (x_clerk_auth_reason=session-token-iat-before-client-uat)，从而导致 action 500。
	// 因此这里优先使用 session JWT 的 iat 作为 __client_uat（或将传入值 clamp 到 iat）。
	jwtIat := parseJWTIAT(auth.SessionJWT)
	effectiveUat := strings.TrimSpace(auth.ClientUat)
	if jwtIat > 0 {
		if effectiveUat == "" {
			effectiveUat = fmt.Sprintf("%d", jwtIat)
		} else if n, err := parseInt64(effectiveUat); err == nil && n > jwtIat {
			effectiveUat = fmt.Sprintf("%d", jwtIat)
		}
	}
	// Clerk 中间件需要 __client_uat 才会把 session 视为有效登录态。
	// 没有这个 cookie 时，即便 actionID 正确，也会直接返回 500（signed-out）。
	if effectiveUat != "" {
		req.AddCookie(&http.Cookie{Name: "__client_uat", Value: effectiveUat})
	} else {
		req.AddCookie(&http.Cookie{Name: "__client_uat", Value: fmt.Sprintf("%d", time.Now().Unix())})
	}
	if auth.ClientJWT != "" {
		req.AddCookie(&http.Cookie{Name: "__client", Value: auth.ClientJWT})
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch credits: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		preview := strings.TrimSpace(string(body))
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		return nil, &creditsRequestError{
			ActionID:    actionID,
			StatusCode:  resp.StatusCode,
			BodyPreview: preview,
			AuthStatus:  strings.TrimSpace(resp.Header.Get("x-clerk-auth-status")),
			AuthReason:  strings.TrimSpace(resp.Header.Get("x-clerk-auth-reason")),
			AuthMessage: strings.TrimSpace(resp.Header.Get("x-clerk-auth-message")),
			VercelError: strings.TrimSpace(resp.Header.Get("x-vercel-error")),
			ContentType: strings.TrimSpace(resp.Header.Get("content-type")),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return parseRSCCredits(string(body))
}

func parseJWTIAT(jwt string) int64 {
	jwt = strings.TrimSpace(jwt)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var data struct {
		IAT int64 `json:"iat"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return 0
	}
	return data.IAT
}

func parseJWTExp(jwt string) int64 {
	jwt = strings.TrimSpace(jwt)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var data struct {
		EXP int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return 0
	}
	return data.EXP
}

func refreshClerkSessionJWT(ctx context.Context, sessionID, clientJWT, clientUat string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	clientJWT = strings.TrimSpace(clientJWT)
	clientUat = strings.TrimSpace(clientUat)
	if sessionID == "" || clientJWT == "" {
		return "", fmt.Errorf("missing sessionID/clientJWT")
	}
	if clientUat == "" {
		clientUat = fmt.Sprintf("%d", time.Now().Unix())
	}

	endpoint := fmt.Sprintf("%s/v1/client/sessions/%s/tokens?__clerk_api_version=%s&_clerk_js_version=%s",
		clerk.ClerkBaseURL, sessionID, clerk.ClerkAPIVersion, clerk.ClerkJSVersion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("organization_id="))
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	req.Header.Set("Cookie", "__client="+clientJWT+"; __client_uat="+clientUat)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("clerk token refresh failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.JWT) == "" {
		return "", fmt.Errorf("clerk token refresh: empty jwt")
	}
	return out.JWT, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not int")
		}
		n = n*10 + int64(ch-'0')
		if n < 0 {
			return 0, fmt.Errorf("overflow")
		}
	}
	return n, nil
}

func getCreditsActionID(ctx context.Context, forceRefresh bool) (string, error) {
	creditsActionIDMu.Lock()
	if !forceRefresh && cachedCreditsActionID != "" && time.Now().Before(cachedCreditsActionExp) {
		id := cachedCreditsActionID
		creditsActionIDMu.Unlock()
		return id, nil
	}
	creditsActionIDMu.Unlock()

	val, err, _ := creditsActionIDSF.Do("discover", func() (interface{}, error) {
		id, err := discoverCreditsActionID(ctx)
		if err != nil {
			return "", err
		}

		creditsActionIDMu.Lock()
		cachedCreditsActionID = id
		cachedCreditsActionExp = time.Now().Add(6 * time.Hour)
		creditsActionIDMu.Unlock()

		return id, nil
	})
	if err != nil {
		return "", err
	}
	return val.(string), nil
}

func discoverCreditsActionID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 8 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orchidsAppURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建 Orchids 首页请求失败: %w", err)
	}
	req.Header.Set("User-Agent", orchidsUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("拉取 Orchids 首页失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("读取 Orchids 首页失败: %w", err)
	}
	body := string(bodyBytes)

	baseURL, _ := url.Parse(orchidsAppURL)
	matches := scriptSrcPattern.FindAllStringSubmatch(body, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		src := strings.TrimSpace(m[1])
		if src == "" {
			continue
		}
		// 只解析 Orchids 自己的 Next chunks，跳过 Clerk 等第三方脚本。
		if !strings.HasPrefix(src, "/_next/") {
			continue
		}

		u, err := baseURL.Parse(src)
		if err != nil {
			continue
		}

		js, err := fetchText(ctx, client, u.String())
		if err != nil {
			continue
		}
		if id := extractCreditsActionIDFromJS(js); id != "" {
			return id, nil
		}
	}

	return "", fmt.Errorf("未能在 Orchids 脚本中解析 credits actionID")
}

func fetchText(ctx context.Context, client *http.Client, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", orchidsUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	// JS chunk 可能很大，这里限制读取大小，避免内存尖峰。
	b, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func extractCreditsActionIDFromJS(js string) string {
	m := serverActionRefPattern.FindStringSubmatch(js)
	if len(m) < 3 {
		return ""
	}
	// m[2] 是 action 名称；这里我们只需要 ID。
	return m[1]
}

// parseRSCCredits parses the RSC text/x-component response to extract credits info.
// RSC format has lines like: 0:{"credits":96571,"plan":"PRO",...}
// or the JSON may be embedded in various RSC chunk formats.
func parseRSCCredits(body string) (*CreditsInfo, error) {
	// Strategy 1: Look for JSON objects containing "credits" field
	matches := jsonObjectPattern.FindAllString(body, -1)
	for _, match := range matches {
		var info CreditsInfo
		if err := json.Unmarshal([]byte(match), &info); err == nil && strings.TrimSpace(info.Plan) != "" {
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
			if err := json.Unmarshal([]byte(data), &info); err == nil && strings.TrimSpace(info.Plan) != "" {
				return &info, nil
			}
			// Try parsing as array element
			var arr []json.RawMessage
			if err := json.Unmarshal([]byte(data), &arr); err == nil {
				for _, elem := range arr {
					if err := json.Unmarshal(elem, &info); err == nil && strings.TrimSpace(info.Plan) != "" {
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
								if err := json.Unmarshal([]byte(candidate), &info); err == nil && strings.TrimSpace(info.Plan) != "" {
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
