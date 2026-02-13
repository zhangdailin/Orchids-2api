package grok

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func randomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func buildStatsigID() string {
	msg := "e:TypeError: Cannot read properties of undefined (reading 'childNodes')"
	return base64.StdEncoding.EncodeToString([]byte(msg))
}

func parseUpstreamLines(body io.Reader, onLine func(map[string]interface{}) error) error {
	decoder := json.NewDecoder(body)
	for {
		var raw map[string]interface{}
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		result, ok := raw["result"].(map[string]interface{})
		if !ok {
			continue
		}
		resp, ok := result["response"].(map[string]interface{})
		if !ok {
			continue
		}
		if err := onLine(resp); err != nil {
			return err
		}
	}
}

func extractImageURLs(value interface{}) []string {
	seen := map[string]struct{}{}
	var out []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lk := strings.ToLower(k)
				if lk == "generatedimageurls" || lk == "imageurls" || lk == "image_urls" {
					switch vv := item.(type) {
					case []interface{}:
						for _, one := range vv {
							if s, ok := one.(string); ok && s != "" {
								if _, exists := seen[s]; !exists {
									seen[s] = struct{}{}
									out = append(out, s)
								}
							}
						}
					case string:
						if vv != "" {
							if _, exists := seen[vv]; !exists {
								seen[vv] = struct{}{}
								out = append(out, vv)
							}
						}
					}
					continue
				}
				walk(item)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(value)
	return out
}

// extractRenderableImageLinks is a broad fallback for Grok tool/card payloads.
// Some Grok responses include image card/tool metadata where URLs aren't under the known keys.
// We conservatively collect http(s) links that look like images and point to Grok-related hosts.
type SearchImagesArgs struct {
	ImageDescription string `json:"image_description"`
	NumberOfImages   int    `json:"number_of_images"`
}

func collectHTTPStrings(value interface{}, limit int) []string {
	out := make([]string, 0, 16)
	seen := map[string]struct{}{}
	var walk func(v interface{})
	walk = func(v interface{}) {
		if limit > 0 && len(out) >= limit {
			return
		}
		switch x := v.(type) {
		case map[string]interface{}:
			for _, vv := range x {
				walk(vv)
			}
		case []interface{}:
			for _, vv := range x {
				walk(vv)
			}
		case string:
			s := strings.TrimSpace(x)
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				if _, ok := seen[s]; !ok {
					seen[s] = struct{}{}
					out = append(out, s)
				}
			}
		}
	}
	walk(value)
	return out
}

func parseSearchImagesArgsFromText(text string) []SearchImagesArgs {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// Match tool cards like: xai:tool_name search_images ... xai:tool_args<![CDATA[{...}]]></xai:tool_args>
	re := regexp.MustCompile(`(?is)xai:tool_name\s*search_images.*?xai:tool_args\s*<!\[CDATA\[(\{.*?\})\]\]>\s*</xai:tool_args>`)
	matches := re.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]SearchImagesArgs, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(m[1])
		if raw == "" {
			continue
		}
		var args SearchImagesArgs
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			continue
		}
		args.ImageDescription = strings.TrimSpace(args.ImageDescription)
		if args.NumberOfImages <= 0 {
			args.NumberOfImages = 4
		}
		if args.ImageDescription != "" {
			out = append(out, args)
		}
	}
	return out
}

func stripToolAndRenderMarkup(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	// Remove xai tool cards (some upstream variants omit the leading '<')
	text = regexp.MustCompile(`(?is)<xai:tool_usage_card.*?</xai:tool_usage_card>`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?is)xai:tool_usage_card.*?</xai:tool_usage_card>`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?is)xai:tool_usage_card.*?(?:</xai:tool_usage_card>|\z)`).ReplaceAllString(text, "")
	// Remove grok render tags (allow optional leading '<')
	text = regexp.MustCompile(`(?is)<?grok:render.*?</grok:render>`).ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func extractRenderableImageLinks(value interface{}) []string {
	seen := map[string]struct{}{}
	var out []string

	isLikelyImageURL := func(s string) bool {
		s = strings.TrimSpace(s)
		ls := strings.ToLower(s)
		if !strings.HasPrefix(ls, "http://") && !strings.HasPrefix(ls, "https://") {
			return false
		}
		// Host allowlist-ish: Grok assets or Grok domain, otherwise ignore to avoid spamming random links.
		if !strings.Contains(ls, "grok.com") && !strings.Contains(ls, "x.ai") {
			return false
		}
		// Common image extensions or Grok CDN patterns.
		if strings.Contains(ls, "assets.grok.com") {
			return true
		}
		for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
			if strings.Contains(ls, ext) {
				return true
			}
		}
		return false
	}

	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for _, item := range x {
				walk(item)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		case string:
			// Some fields may contain multiple URLs; split on whitespace.
			for _, part := range strings.Fields(x) {
				p := strings.Trim(part, "\"'()[]{}<>,")
				if isLikelyImageURL(p) {
					if _, ok := seen[p]; !ok {
						seen[p] = struct{}{}
						out = append(out, p)
					}
				}
			}
		}
	}

	walk(value)
	return out
}

type AttachmentInput struct {
	Type string
	Data string
}

func extractMessageAndAttachments(messages []ChatMessage, isVideo bool) (string, []AttachmentInput, error) {
	flatten := make([]struct {
		Role string
		Text string
	}, 0, len(messages))
	attachments := make([]AttachmentInput, 0)

	for _, msg := range messages {
		role := normalizeMessageRole(msg.Role)
		switch content := msg.Content.(type) {
		case string:
			text := strings.TrimSpace(content)
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		case []interface{}:
			var parts []string
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						parts = append(parts, strings.TrimSpace(s))
					}
				case "image_url":
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "image", Data: data})
					}
				case "file":
					if isVideo {
						return "", nil, fmt.Errorf("video model does not support file content blocks")
					}
					if data := extractAttachmentURL(m["file"]); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "file", Data: data})
					}
				case "input_audio":
					if isVideo {
						return "", nil, fmt.Errorf("video model does not support input_audio content blocks")
					}
					if data := extractAttachmentData(m["input_audio"], "data"); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "audio", Data: data})
					}
				}
			}
			text := strings.TrimSpace(strings.Join(parts, "\n"))
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		default:
			text := strings.TrimSpace(extractContentText(content))
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		}
	}

	lastUser := -1
	for i := len(flatten) - 1; i >= 0; i-- {
		if flatten[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if len(flatten) == 0 {
		return "", attachments, nil
	}

	var parts []string
	for i, item := range flatten {
		if i == lastUser {
			parts = append(parts, item.Text)
			continue
		}
		role := item.Role
		if role == "" {
			role = "user"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", role, item.Text))
	}
	return strings.Join(parts, "\n\n"), attachments, nil
}

func extractContentText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if strings.EqualFold(fmt.Sprint(m["type"]), "text") {
				if s, ok := m["text"].(string); ok {
					s = strings.TrimSpace(s)
					if s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func writeSSE(w http.ResponseWriter, event, data string) {
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func parseTokenValue(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	if strings.Contains(token, "sso=") {
		idx := strings.Index(token, "sso=")
		if idx >= 0 {
			token = token[idx+len("sso="):]
			if semi := strings.Index(token, ";"); semi >= 0 {
				token = token[:semi]
			}
		}
	}
	return strings.TrimSpace(token)
}

func parseRateLimitInfo(headers http.Header) *RateLimitInfo {
	if headers == nil {
		return nil
	}
	limitRaw := firstHeaderValue(
		headers,
		"ratelimit-limit",
		"x-ratelimit-limit",
		"x-rate-limit-limit",
		"x-usage-limit",
		"x-ratelimit-limit-requests",
		"x-ratelimit-limit-reqs",
	)
	remainingRaw := firstHeaderValue(
		headers,
		"ratelimit-remaining",
		"x-ratelimit-remaining",
		"x-rate-limit-remaining",
		"x-usage-remaining",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-remaining-reqs",
	)
	resetRaw := firstHeaderValue(
		headers,
		"ratelimit-reset",
		"x-ratelimit-reset",
		"x-rate-limit-reset",
		"x-ratelimit-reset-requests",
	)

	limit, okLimit := parseRateLimitValue(limitRaw)
	remaining, okRemaining := parseRateLimitValue(remainingRaw)
	resetAt := parseRateLimitReset(resetRaw)

	if !okLimit && !okRemaining && resetRaw == "" {
		return nil
	}

	info := &RateLimitInfo{
		Limit:     limit,
		Remaining: remaining,
		ResetAt:   resetAt,
	}
	return info
}

func parseRateLimitPayload(payload map[string]interface{}) *RateLimitInfo {
	if payload == nil {
		return nil
	}

	limitKeys := map[string]struct{}{
		"limit":            {},
		"limit_tokens":     {},
		"limittokens":      {},
		"max_tokens":       {},
		"maxtokens":        {},
		"token_limit":      {},
		"tokenlimit":       {},
		"tokens_limit":     {},
		"tokenslimit":      {},
		"total_tokens":     {},
		"totaltokens":      {},
		"quota":            {},
		"quota_limit":      {},
		"quotalimit":       {},
		"request_limit":    {},
		"requestlimit":     {},
		"requests_limit":   {},
		"requestslimit":    {},
		"request_limiters": {},
	}
	remainingKeys := map[string]struct{}{
		"remaining":          {},
		"remaining_tokens":   {},
		"remainingtokens":    {},
		"tokens_remaining":   {},
		"tokensremaining":    {},
		"quota_remaining":    {},
		"quotaremaining":     {},
		"remaining_requests": {},
		"remainingrequests":  {},
	}
	resetKeys := map[string]struct{}{
		"reset":           {},
		"reset_at":        {},
		"resetat":         {},
		"reset_at_ms":     {},
		"resetatms":       {},
		"reset_time":      {},
		"resettime":       {},
		"reset_timestamp": {},
		"resettimestamp":  {},
		"next_reset":      {},
		"nextreset":       {},
	}

	limit, okLimit := findNumberByKeys(payload, limitKeys)
	remaining, okRemaining := findNumberByKeys(payload, remainingKeys)
	resetRaw, okReset := findValueByKeys(payload, resetKeys)

	if !okLimit && !okRemaining && !okReset {
		return nil
	}

	info := &RateLimitInfo{
		Limit:     limit,
		Remaining: remaining,
	}
	if okReset {
		info.ResetAt = parseRateLimitReset(fmt.Sprint(resetRaw))
	}
	return info
}

func classifyAccountStatusFromError(errStr string) string {
	lower := strings.ToLower(errStr)
	switch {
	case hasExplicitHTTPStatus(lower, "401") || strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out") || strings.Contains(lower, "unauthorized"):
		return "401"
	case hasExplicitHTTPStatus(lower, "403") || strings.Contains(lower, "forbidden"):
		return "403"
	case hasExplicitHTTPStatus(lower, "404"):
		return "404"
	case hasExplicitHTTPStatus(lower, "429") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "no remaining quota") ||
		strings.Contains(lower, "out of credits") ||
		strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "run out of credits"):
		return "429"
	default:
		return ""
	}
}

func hasExplicitHTTPStatus(lower string, code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || lower == "" {
		return false
	}
	patterns := []string{
		"http " + code,
		"http/1.1 " + code,
		"http/2 " + code,
		"status " + code,
		"status=" + code,
		"status:" + code,
		"statuscode " + code,
		"statuscode=" + code,
		"status code " + code,
		"code " + code,
		"code=" + code,
		"code:" + code,
		"response status " + code,
		"response code " + code,
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func findNumberByKeys(value interface{}, keys map[string]struct{}) (int64, bool) {
	raw, ok := findValueByKeys(value, keys)
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	case string:
		return parseRateLimitValue(v)
	default:
		return parseRateLimitValue(fmt.Sprint(v))
	}
}

func findValueByKeys(value interface{}, keys map[string]struct{}) (interface{}, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		for k, item := range v {
			if _, ok := keys[normalizeRateKey(k)]; ok {
				return item, true
			}
		}
		for _, item := range v {
			if out, ok := findValueByKeys(item, keys); ok {
				return out, true
			}
		}
	case []interface{}:
		for _, item := range v {
			if out, ok := findValueByKeys(item, keys); ok {
				return out, true
			}
		}
	}
	return nil, false
}

func normalizeRateKey(key string) string {
	key = strings.TrimSpace(strings.ToLower(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "")
	return key
}

func firstHeaderValue(headers http.Header, keys ...string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if val := strings.TrimSpace(headers.Get(key)); val != "" {
			return val
		}
	}
	return ""
}

func parseRateLimitValue(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return v, true
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return int64(f), true
	}
	return 0, false
}

func parseRateLimitReset(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if v, ok := parseRateLimitValue(raw); ok {
		// Treat large values as milliseconds.
		if v > 1_000_000_000_000 {
			return time.UnixMilli(v)
		}
		if v > 0 {
			return time.Unix(v, 0)
		}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func encodeJSON(v interface{}) string {
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimSpace(buf.String())
}

func normalizeMessageRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "function":
		return "tool"
	case "":
		return "user"
	default:
		return role
	}
}

func extractAttachmentURL(v interface{}) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]interface{}:
		if u, ok := x["url"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
		if u, ok := x["data"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

func extractAttachmentData(v interface{}, field string) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]interface{}:
		if u, ok := x[field].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

func dataURIFromBytes(mime string, data []byte) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func parseDataURI(input string) (fileName, contentBase64, mime string, err error) {
	s := strings.TrimSpace(input)
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return "", "", "", errors.New("not a data uri")
	}
	idx := strings.Index(s, ",")
	if idx <= 0 {
		return "", "", "", errors.New("invalid data uri")
	}
	header := s[5:idx]
	payload := strings.TrimSpace(s[idx+1:])
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return "", "", "", errors.New("data uri is not base64 encoded")
	}
	mime = strings.TrimSpace(strings.Split(header, ";")[0])
	if mime == "" {
		mime = "application/octet-stream"
	}
	ext := "bin"
	if slash := strings.Index(mime, "/"); slash >= 0 && slash+1 < len(mime) {
		ext = strings.TrimSpace(mime[slash+1:])
	}
	return "file." + ext, payload, mime, nil
}

func isRemoteURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return false
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	return scheme == "http" || scheme == "https"
}

func fetchRemoteAsDataURI(rawURL string, timeout time.Duration) (string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "", fmt.Errorf("empty url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("fetch url status=%d body=%s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 60*1024*1024))
	if err != nil {
		return "", err
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mime == "" {
		mime = mimeFromFilename(u)
	}
	return dataURIFromBytes(mime, data), nil
}

func mimeFromFilename(name string) string {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(name)))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func uniqueStrings(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, s := range input {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func parseBoolLoose(raw string, fallback bool) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func parseIntLoose(raw string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return v
}

func resolveAspectRatio(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "16:9", "9:16", "1:1", "2:3", "3:2":
		return size
	case "1024x1024", "512x512":
		return "1:1"
	case "1024x576", "1280x720", "1536x864":
		return "16:9"
	case "576x1024", "720x1280", "864x1536":
		return "9:16"
	case "1024x1536", "512x768", "768x1024":
		return "2:3"
	case "1536x1024", "768x512", "1024x768":
		return "3:2"
	default:
		return "2:3"
	}
}

func extractImageProgress(resp map[string]interface{}) (index int, progress int, ok bool) {
	raw, ok := resp["streamingImageGenerationResponse"].(map[string]interface{})
	if !ok {
		return 0, 0, false
	}
	return interfaceToInt(raw["imageIndex"]), interfaceToInt(raw["progress"]), true
}

func extractVideoProgress(resp map[string]interface{}) (progress int, videoURL, thumbnailURL string, ok bool) {
	raw, ok := resp["streamingVideoGenerationResponse"].(map[string]interface{})
	if !ok {
		return 0, "", "", false
	}
	return interfaceToInt(raw["progress"]), strings.TrimSpace(fmt.Sprint(raw["videoUrl"])), strings.TrimSpace(fmt.Sprint(raw["thumbnailImageUrl"])), true
}

func interfaceToInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return i
		}
	}
	return 0
}
