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
	"sort"
	"strconv"
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
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
				if lk == "generatedimageurls" || lk == "imageurls" || lk == "image_urls" || lk == "imageurl" {
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

func collectAssetLikeStrings(value interface{}, limit int) []string {
	out := make([]string, 0, 32)
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
			if s == "" {
				return
			}
			ls := strings.ToLower(s)
			// Common patterns when no direct URL is provided.
			looksAsset := strings.Contains(ls, "assets") || strings.Contains(ls, "grok") || strings.Contains(ls, ".jpg") || strings.Contains(ls, ".png") || strings.Contains(ls, ".webp")
			looksPath := strings.HasPrefix(s, "/") && (strings.Contains(ls, "image") || strings.Contains(ls, "asset") || strings.Contains(ls, "."))
			if looksAsset || looksPath {
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

func stripToolAndRenderMarkup(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	// Convert xai tool cards into readable text.
	text = regexp.MustCompile(`(?is)<?xai:tool_usage_card[^>]*>.*?</xai:tool_usage_card>`).ReplaceAllStringFunc(text, func(raw string) string {
		line := extractToolUsageCardText(raw)
		if line == "" {
			return ""
		}
		return "\n" + line + "\n"
	})
	// Drop incomplete tool cards.
	text = regexp.MustCompile(`(?is)<?xai:tool_usage_card.*?(?:</xai:tool_usage_card>|\z)`).ReplaceAllString(text, "")
	// Remove grok render tags (allow optional leading '<')
	text = regexp.MustCompile(`(?is)<?grok:render.*?</grok:render>`).ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func extractToolUsageCardText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	tagText := func(input, tag string) string {
		pat := fmt.Sprintf(`(?is)<%s>(.*?)</%s>`, regexp.QuoteMeta(tag), regexp.QuoteMeta(tag))
		m := regexp.MustCompile(pat).FindStringSubmatch(input)
		if len(m) < 2 {
			return ""
		}
		val := strings.TrimSpace(m[1])
		// unwrap <![CDATA[...]]>
		val = regexp.MustCompile(`(?is)<!\[CDATA\[(.*?)\]\]>`).ReplaceAllString(val, "$1")
		return strings.TrimSpace(val)
	}
	name := tagText(raw, "xai:tool_name")
	argsRaw := tagText(raw, "xai:tool_args")

	var payload map[string]interface{}
	if strings.TrimSpace(argsRaw) != "" {
		_ = json.Unmarshal([]byte(argsRaw), &payload)
	}
	read := func(keys ...string) string {
		for _, key := range keys {
			v, _ := payload[key]
			s := strings.TrimSpace(fmt.Sprint(v))
			if s != "" && s != "<nil>" {
				return s
			}
		}
		return ""
	}

	label := strings.TrimSpace(name)
	text := strings.TrimSpace(argsRaw)
	switch label {
	case "web_search":
		label = "[WebSearch]"
		if s := read("query", "q"); s != "" {
			text = s
		}
	case "search_images":
		label = "[SearchImage]"
		if s := read("image_description", "description", "query"); s != "" {
			text = s
		}
	case "chatroom_send":
		label = "[AgentThink]"
		if s := read("message"); s != "" {
			text = s
		}
	}

	switch {
	case label != "" && text != "":
		return strings.TrimSpace(label + " " + text)
	case label != "":
		return label
	case text != "":
		return text
	default:
		// Fallback: strip tags and return plain text if any.
		return strings.TrimSpace(regexp.MustCompile(`(?is)<[^>]+>`).ReplaceAllString(raw, ""))
	}
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

	// Prefer higher-quality originals over tiny thumbnails.
	score := func(u string) int {
		lu := strings.ToLower(u)
		// Google tbn thumbnails are almost always low-res.
		if strings.Contains(lu, "encrypted-tbn0.gstatic.com") {
			return 0
		}
		if strings.Contains(lu, "thumbnail") || strings.Contains(lu, "thumb") {
			return 10
		}
		// Prefer URLs that look like direct image files.
		if strings.Contains(lu, ".jpg") || strings.Contains(lu, ".jpeg") || strings.Contains(lu, ".png") || strings.Contains(lu, ".webp") || strings.Contains(lu, ".gif") {
			return 100
		}
		return 50
	}
	sort.SliceStable(out, func(i, j int) bool {
		si := score(out[i])
		sj := score(out[j])
		if si == sj {
			return out[i] < out[j]
		}
		return si > sj
	})

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

func extractLastUserText(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if normalizeMessageRole(msg.Role) != "user" {
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			return strings.TrimSpace(content)
		case []interface{}:
			parts := make([]string, 0, len(content))
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if strings.EqualFold(fmt.Sprint(m["type"]), "text") {
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						parts = append(parts, strings.TrimSpace(s))
					}
				}
			}
			return strings.TrimSpace(strings.Join(parts, "\n"))
		default:
			return strings.TrimSpace(extractContentText(content))
		}
	}
	return ""
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

// NormalizeSSOToken extracts the raw SSO token from a cookie-like string.
func NormalizeSSOToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}
	lower := strings.ToLower(token)
	if idx := strings.Index(lower, "sso="); idx >= 0 {
		token = token[idx+len("sso="):]
		if semi := strings.Index(token, ";"); semi >= 0 {
			token = token[:semi]
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
		"max_queries":      {},
		"maxqueries":       {},
		"query_limit":      {},
		"querylimit":       {},
		"queries_limit":    {},
		"querieslimit":     {},
		"token_limit":      {},
		"tokenlimit":       {},
		"tokens_limit":     {},
		"tokenslimit":      {},
		"total_tokens":     {},
		"totaltokens":      {},
		"total_queries":    {},
		"totalqueries":     {},
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
		"remaining_queries":  {},
		"remainingqueries":   {},
		"queries_remaining":  {},
		"queriesremaining":   {},
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

// classifyAccountStatusFromError delegates to the centralized errors package.
func classifyAccountStatusFromError(errStr string) string {
	return apperrors.ClassifyAccountStatus(errStr)
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

func validateChatMessages(messages []ChatMessage) error {
	allowedRoles := map[string]struct{}{
		"developer": {},
		"system":    {},
		"user":      {},
		"assistant": {},
	}
	userContentTypes := map[string]struct{}{
		"text":        {},
		"image_url":   {},
		"input_audio": {},
		"file":        {},
	}

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if _, ok := allowedRoles[role]; !ok {
			return fmt.Errorf("role must be one of [assistant developer system user]")
		}
		switch content := msg.Content.(type) {
		case string:
			if strings.TrimSpace(content) == "" {
				return fmt.Errorf("message content cannot be empty")
			}
		case []interface{}:
			if len(content) == 0 {
				return fmt.Errorf("message content cannot be an empty array")
			}
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					return fmt.Errorf("content block must be an object")
				}
				if len(m) == 0 {
					return fmt.Errorf("content block cannot be empty")
				}
				rawType, hasType := m["type"]
				if !hasType {
					return fmt.Errorf("content block must have a 'type' field")
				}
				blockType := strings.TrimSpace(fmt.Sprint(rawType))
				if blockType == "" {
					return fmt.Errorf("content block 'type' cannot be empty")
				}

				if role == "user" {
					if _, ok := userContentTypes[blockType]; !ok {
						return fmt.Errorf("invalid content block type: '%s'", blockType)
					}
				} else if blockType != "text" {
					return fmt.Errorf("the '%s' role only supports 'text' type, got '%s'", role, blockType)
				}

				switch blockType {
				case "text":
					text, _ := m["text"].(string)
					if strings.TrimSpace(text) == "" {
						return fmt.Errorf("text content cannot be empty")
					}
				case "image_url":
					imageURL, _ := m["image_url"].(map[string]interface{})
					if imageURL == nil {
						return fmt.Errorf("image_url must have a 'url' field")
					}
					urlVal, _ := imageURL["url"].(string)
					if err := validateMediaInput(urlVal, "image_url.url"); err != nil {
						return err
					}
				case "input_audio":
					audio, _ := m["input_audio"].(map[string]interface{})
					if audio == nil {
						return fmt.Errorf("input_audio must have a 'data' field")
					}
					dataVal, _ := audio["data"].(string)
					if err := validateMediaInput(dataVal, "input_audio.data"); err != nil {
						return err
					}
				case "file":
					fileData, _ := m["file"].(map[string]interface{})
					if fileData == nil {
						return fmt.Errorf("file must have a 'file_data' field")
					}
					dataVal, _ := fileData["file_data"].(string)
					if err := validateMediaInput(dataVal, "file.file_data"); err != nil {
						return err
					}
				default:
					return fmt.Errorf("invalid content block type: '%s'", blockType)
				}

			}
		default:
			return fmt.Errorf("message content must be a string or array")
		}
	}
	return nil
}

func validateMediaInput(value string, fieldName string) error {
	val := strings.TrimSpace(value)
	if val == "" {
		return fmt.Errorf("%s cannot be empty", fieldName)
	}
	lower := strings.ToLower(val)
	if strings.HasPrefix(lower, "data:") {
		return nil
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return nil
	}
	if looksLikeBase64Payload(val) {
		return fmt.Errorf("%s base64 must be provided as a data URI (data:<mime>;base64,...)", fieldName)
	}
	return fmt.Errorf("%s must be a URL or data URI", fieldName)
}

func looksLikeBase64Payload(value string) bool {
	candidate := strings.Join(strings.Fields(value), "")
	if len(candidate) < 32 || len(candidate)%4 != 0 {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(candidate)
	return err == nil
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
		if u, ok := x["file_data"].(string); ok && strings.TrimSpace(u) != "" {
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

func extractPromptAndImageURLs(messages []ChatMessage) (string, []string) {
	prompt := ""
	imageURLs := make([]string, 0)

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		switch content := msg.Content.(type) {
		case string:
			text := strings.TrimSpace(content)
			if text != "" {
				prompt = text
			}
		case []interface{}:
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						prompt = strings.TrimSpace(s)
					}
				case "image_url":
					if role != "user" {
						continue
					}
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						imageURLs = append(imageURLs, data)
					}
				}
			}
		default:
			text := strings.TrimSpace(extractContentText(content))
			if text != "" {
				prompt = text
			}
		}
	}
	return prompt, imageURLs
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

func fetchRemoteAsDataURI(rawURL string, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) (string, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "", fmt.Errorf("empty url")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	if proxyFunc != nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = proxyFunc
		client.Transport = transport
	}
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

func validateVideoConfig(cfg *VideoConfig) (*VideoConfig, error) {
	if cfg == nil {
		cfg = &VideoConfig{}
	}
	cfg.Normalize()

	aspectRatioMap := map[string]string{
		"1280x720":  "16:9",
		"720x1280":  "9:16",
		"1792x1024": "3:2",
		"1024x1792": "2:3",
		"1024x1024": "1:1",
		"16:9":      "16:9",
		"9:16":      "9:16",
		"3:2":       "3:2",
		"2:3":       "2:3",
		"1:1":       "1:1",
	}
	ar := strings.TrimSpace(cfg.AspectRatio)
	if ar == "" {
		ar = "3:2"
	}
	mapped, ok := aspectRatioMap[ar]
	if !ok {
		return nil, fmt.Errorf("aspect_ratio must be one of [1280x720 720x1280 1792x1024 1024x1792 1024x1024 16:9 9:16 3:2 2:3 1:1]")
	}
	cfg.AspectRatio = mapped

	if cfg.VideoLength != 6 && cfg.VideoLength != 10 && cfg.VideoLength != 15 {
		return nil, fmt.Errorf("video_length must be 6, 10, or 15 seconds")
	}
	resolution := strings.TrimSpace(cfg.ResolutionName)
	if resolution != "480p" && resolution != "720p" {
		return nil, fmt.Errorf("resolution_name must be one of ['480p', '720p']")
	}
	cfg.ResolutionName = resolution

	preset := strings.TrimSpace(cfg.Preset)
	switch preset {
	case "fun", "normal", "spicy", "custom":
	default:
		return nil, fmt.Errorf("preset must be one of ['fun', 'normal', 'spicy', 'custom']")
	}
	cfg.Preset = preset
	return cfg, nil
}

func normalizeImageSize(size string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return "1024x1024", nil
	}
	switch s {
	case "1280x720", "720x1280", "1792x1024", "1024x1792", "1024x1024":
		return s, nil
	default:
		return "", fmt.Errorf("size must be one of 1280x720/720x1280/1792x1024/1024x1792/1024x1024")
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
