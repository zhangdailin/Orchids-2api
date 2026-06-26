package grok

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/util"
)

var (
	grokJSONEmptyObjectBytes  = []byte("{}")
	grokSSEEventPrefixBytes   = []byte("event: ")
	grokSSEDataPrefixBytes    = []byte("data: ")
	grokSSENewlineBytes       = []byte("\n")
	grokSSEFrameSuffixBytes   = []byte("\n\n")
	reToolUsageCardBlock      = regexp.MustCompile(`(?is)<?xai:tool_usage_card[^>]*>.*?</xai:tool_usage_card>`)
	reToolUsageCardIncomplete = regexp.MustCompile(`(?is)<?xai:tool_usage_card.*?(?:</xai:tool_usage_card>|\z)`)
	reGrokRenderBlock         = regexp.MustCompile(`(?is)<?grok:render.*?</grok:render>`)

	rateLimitFamilies = []rateLimitFieldFamily{
		{
			unit: "tokens",
			limitKeys: []string{
				"limit_tokens",
				"limittokens",
				"max_tokens",
				"maxtokens",
				"token_limit",
				"tokenlimit",
				"tokens_limit",
				"tokenslimit",
				"total_tokens",
				"totaltokens",
			},
			remainingKeys: []string{
				"remaining_tokens",
				"remainingtokens",
				"tokens_remaining",
				"tokensremaining",
			},
		},
		{
			unit: "requests",
			limitKeys: []string{
				"max_queries",
				"maxqueries",
				"query_limit",
				"querylimit",
				"queries_limit",
				"querieslimit",
				"total_queries",
				"totalqueries",
				"request_limit",
				"requestlimit",
				"requests_limit",
				"requestslimit",
			},
			remainingKeys: []string{
				"remaining_queries",
				"remainingqueries",
				"queries_remaining",
				"queriesremaining",
				"remaining_requests",
				"remainingrequests",
			},
		},
		{
			unit: "",
			limitKeys: []string{
				"limit",
				"quota",
				"quota_limit",
				"quotalimit",
			},
			remainingKeys: []string{
				"remaining",
				"quota_remaining",
				"quotaremaining",
			},
		},
	}
	rateLimitNumericKeys = buildRateLimitNumericKeySet(rateLimitFamilies)
	rateLimitResetKeys   = map[string]struct{}{
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
	renderableImageExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}
	allowedMessageRoles       = map[string]struct{}{
		"developer": {},
		"system":    {},
		"user":      {},
		"assistant": {},
		"tool":      {},
	}
	userContentTypes = map[string]struct{}{
		"text":        {},
		"image_url":   {},
		"input_audio": {},
		"file":        {},
	}
	videoAspectRatioMap = map[string]string{
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
)

type rateLimitFieldFamily struct {
	unit          string
	limitKeys     []string
	remainingKeys []string
}

func buildRateLimitNumericKeySet(families []rateLimitFieldFamily) map[string]struct{} {
	out := make(map[string]struct{})
	for _, family := range families {
		for _, key := range family.limitKeys {
			out[key] = struct{}{}
		}
		for _, key := range family.remainingKeys {
			out[key] = struct{}{}
		}
	}
	return out
}

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

func randomUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4],
		buf[4:6],
		buf[6:8],
		buf[8:10],
		buf[10:16],
	)
}

func buildStatsigID() string {
	seed := randomHex(1)
	if seed == "" {
		return base64.StdEncoding.EncodeToString([]byte("x1:TypeError: Cannot read properties of undefined (reading 'children')"))
	}
	if seed[0]%2 == 0 {
		suffix := randomStringFromCharset(5, "abcdefghijklmnopqrstuvwxyz0123456789")
		if suffix == "" {
			suffix = "child"
		}
		return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("x1:TypeError: Cannot read properties of null (reading 'children[\\'%s\\']')", suffix)))
	}
	property := randomStringFromCharset(10, "abcdefghijklmnopqrstuvwxyz")
	if property == "" {
		property = "children"
	}
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("x1:TypeError: Cannot read properties of undefined (reading '%s')", property)))
}

func isBrowserStatsigID(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(decoded), "x1:TypeError: Cannot read properties of ")
}

func randomStringFromCharset(length int, charset string) string {
	if length <= 0 || charset == "" {
		return ""
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = charset[int(b)%len(charset)]
	}
	return string(out)
}

func parseUpstreamLines(body io.Reader, onLine func(map[string]interface{}) error) error {
	decoder := json.NewDecoder(body)
	type upstreamLineEnvelope struct {
		Result struct {
			Response map[string]interface{} `json:"response"`
		} `json:"result"`
	}

	for {
		var line upstreamLineEnvelope
		if err := decoder.Decode(&line); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := line.Result.Response
		if resp == nil {
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
	add := func(raw string) {
		s := normalizeGrokAssetURL(raw)
		if s == "" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lk := strings.ToLower(k)
				if lk == "jsondata" || lk == "cardattachmentsjson" {
					walk(parseGrokJSONData(item))
					continue
				}
				if lk == "generatedimageurls" || lk == "imageurls" || lk == "image_urls" || lk == "imageurl" || lk == "image_url" {
					switch vv := item.(type) {
					case []interface{}:
						for _, one := range vv {
							if s, ok := one.(string); ok && s != "" {
								add(s)
							}
						}
					case string:
						add(vv)
					}
					continue
				}
				walk(item)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		case string:
			if parsed := parseGrokJSONText(x); parsed != nil {
				walk(parsed)
			}
		}
	}
	walk(value)
	return out
}

func parseGrokJSONData(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}, []interface{}:
		return x
	case string:
		raw := strings.TrimSpace(x)
		if raw == "" {
			return nil
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			return parsed
		}
		return x
	default:
		return x
	}
}

func parseGrokJSONText(raw string) interface{} {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		return nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil
	}
	return parsed
}

func normalizeGrokAssetURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "<nil>" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return s
	}
	if strings.HasPrefix(s, "/") {
		return defaultAssetsBaseURL + s
	}
	if strings.HasPrefix(lower, "users/") || strings.HasPrefix(lower, "generated/") || strings.Contains(lower, "/generated/") || strings.Contains(lower, "/image/") {
		return defaultAssetsBaseURL + "/" + strings.TrimLeft(s, "/")
	}
	return s
}

// extractRenderableImageLinks is a broad fallback for Grok tool/card payloads.
// Some Grok responses include image card/tool metadata where URLs aren't under the known keys.
// We conservatively collect http(s) links that look like images and point to Grok-related hosts.

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
	text = reToolUsageCardBlock.ReplaceAllStringFunc(text, func(raw string) string {
		line := extractToolUsageCardText(raw)
		if line == "" {
			return ""
		}
		return "\n" + line + "\n"
	})
	// Drop incomplete tool cards.
	text = reToolUsageCardIncomplete.ReplaceAllString(text, "")
	// Remove grok render tags (allow optional leading '<')
	text = reGrokRenderBlock.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func extractToolUsageCardText(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	rolloutID := strings.TrimSpace(extractToolUsageTagValue(raw, "rolloutId"))
	name := extractToolUsageTagValue(raw, "xai:tool_name")
	argsRaw := extractToolUsageTagValue(raw, "xai:tool_args")

	var payload struct {
		Query            string `json:"query"`
		Q                string `json:"q"`
		ImageDescription string `json:"image_description"`
		Description      string `json:"description"`
		Message          string `json:"message"`
	}
	if argsRaw != "" {
		_ = json.Unmarshal([]byte(argsRaw), &payload)
	}

	label := strings.TrimSpace(name)
	text := strings.TrimSpace(argsRaw)
	prefix := ""
	if rolloutID != "" {
		prefix = "[" + rolloutID + "]"
	}
	switch label {
	case "web_search":
		label = prefix + "[WebSearch]"
		if s := firstNonEmpty(payload.Query, payload.Q); s != "" {
			text = s
		}
	case "search_images":
		label = prefix + "[SearchImage]"
		if s := firstNonEmpty(payload.ImageDescription, payload.Description, payload.Query); s != "" {
			text = s
		}
	case "chatroom_send":
		label = prefix + "[AgentThink]"
		if s := payload.Message; s != "" {
			text = s
		}
	default:
		if label != "" && prefix != "" {
			label = prefix + label
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
		return strings.TrimSpace(stripAngleTags(raw))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func extractToolUsageTagValue(raw, tag string) string {
	if raw == "" || tag == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	openTag := "<" + strings.ToLower(tag) + ">"
	closeTag := "</" + strings.ToLower(tag) + ">"
	start := strings.Index(lower, openTag)
	if start < 0 {
		return ""
	}
	start += len(openTag)
	if start >= len(raw) {
		return ""
	}
	endRel := strings.Index(lower[start:], closeTag)
	if endRel < 0 {
		return ""
	}
	val := strings.TrimSpace(raw[start : start+endRel])
	if val == "" {
		return ""
	}
	trimmed := strings.TrimSpace(val)
	lowerTrimmed := strings.ToLower(trimmed)
	if strings.HasPrefix(lowerTrimmed, "<![cdata[") && strings.HasSuffix(lowerTrimmed, "]]>") && len(trimmed) >= len("<![CDATA[]]>") {
		trimmed = strings.TrimSpace(trimmed[len("<![CDATA[") : len(trimmed)-len("]]>")])
	}
	return strings.TrimSpace(trimmed)
}

func stripAngleTags(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '<':
			inTag = true
		case '>':
			if inTag {
				inTag = false
			} else {
				b.WriteByte(ch)
			}
		default:
			if !inTag {
				b.WriteByte(ch)
			}
		}
	}
	return b.String()
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
		for _, ext := range renderableImageExtensions {
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

func extractMessageAndAttachmentsWithTools(messages []ChatMessage, isVideo bool, tools []ToolDef, toolChoice interface{}, parallelToolCalls bool) (string, []AttachmentInput, error) {
	if len(tools) > 0 {
		messages = formatToolHistory(messages)
	}
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
	combined := strings.Join(parts, "\n\n")
	if strings.TrimSpace(combined) == "" && len(attachments) > 0 {
		combined = "Refer to the following content:"
	}
	if prompt := buildToolPrompt(tools, toolChoice, parallelToolCalls); prompt != "" {
		if strings.TrimSpace(combined) != "" {
			combined = prompt + "\n\n" + combined
		} else {
			combined = prompt
		}
	}
	return combined, attachments, nil
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

func writeSSEError(w http.ResponseWriter, message, errType, code string) {
	payload := map[string]interface{}{
		"error": map[string]interface{}{
			"message": strings.TrimSpace(message),
			"type":    strings.TrimSpace(errType),
			"code":    strings.TrimSpace(code),
		},
	}
	writeSSEBytes(w, "error", encodeJSONBytes(payload))
}

func writeSSEEventName(w http.ResponseWriter, event string) {
	if sw, ok := interface{}(w).(io.StringWriter); ok {
		_, _ = sw.WriteString(event)
		return
	}
	_, _ = w.Write([]byte(event))
}

func writeSSEBytes(w http.ResponseWriter, event string, data []byte) {
	if event != "" {
		_, _ = w.Write(grokSSEEventPrefixBytes)
		writeSSEEventName(w, event)
		_, _ = w.Write(grokSSENewlineBytes)
	}
	_, _ = w.Write(grokSSEDataPrefixBytes)
	_, _ = w.Write(data)
	_, _ = w.Write(grokSSEFrameSuffixBytes)
}

// NormalizeSSOToken extracts the raw SSO token from a cookie-like string.
func NormalizeSSOToken(raw string) string {
	token := strings.TrimSpace(raw)
	if token == "" {
		return ""
	}

	// Cookie-style input: scan pairs and prefer exact "sso" key.
	if strings.Contains(token, ";") {
		parts := strings.Split(token, ";")
		for _, part := range parts {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(kv[0]), "sso") {
				return strings.TrimSpace(kv[1])
			}
		}
		return strings.TrimSpace(token)
	}

	// Plain "sso=<token>" input.
	lower := strings.ToLower(strings.TrimSpace(token))
	if strings.HasPrefix(lower, "sso=") {
		return strings.TrimSpace(token[len("sso="):])
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
		Limit:        limit,
		HasLimit:     okLimit,
		Remaining:    remaining,
		HasRemaining: okRemaining,
		ResetAt:      resetAt,
		Unit:         "requests",
	}
	return info
}

func parseRateLimitPayload(payload map[string]interface{}) *RateLimitInfo {
	if payload == nil {
		return nil
	}

	numericFields := make(map[string]int64)
	resetFields := make(map[string]time.Time)
	collectRateLimitPayloadFields(payload, numericFields, resetFields)

	info := buildRateLimitInfoFromFields(numericFields, resetFields)
	if info == nil {
		return nil
	}
	return info
}

// classifyAccountStatusFromError delegates to the centralized errors package.
func classifyAccountStatusFromError(errStr string) string {
	return apperrors.ClassifyAccountStatus(errStr)
}

func collectRateLimitPayloadFields(value interface{}, numericFields map[string]int64, resetFields map[string]time.Time) {
	switch v := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			item := v[k]
			key := normalizeRateKey(k)
			if _, ok := rateLimitNumericKeys[key]; ok {
				if _, seen := numericFields[key]; !seen {
					if n, ok := parseNumberAny(item); ok {
						numericFields[key] = n
					}
				}
			}
			if _, ok := rateLimitResetKeys[key]; ok {
				if _, seen := resetFields[key]; !seen {
					if t := parseRateLimitReset(fmt.Sprint(item)); !t.IsZero() {
						resetFields[key] = t
					}
				}
			}
		}
		for _, k := range keys {
			collectRateLimitPayloadFields(v[k], numericFields, resetFields)
		}
	case []interface{}:
		for _, item := range v {
			collectRateLimitPayloadFields(item, numericFields, resetFields)
		}
	}
}

func buildRateLimitInfoFromFields(numericFields map[string]int64, resetFields map[string]time.Time) *RateLimitInfo {
	var (
		best         *RateLimitInfo
		bestRank     = -1
		bestComplete bool
	)

	for idx, family := range rateLimitFamilies {
		limit, hasLimit := firstRateLimitNumeric(numericFields, family.limitKeys)
		remaining, hasRemaining := firstRateLimitNumeric(numericFields, family.remainingKeys)
		if !hasLimit && !hasRemaining {
			continue
		}
		complete := hasLimit && hasRemaining
		if best != nil {
			if bestComplete && !complete {
				continue
			}
			if bestComplete == complete && bestRank <= idx {
				continue
			}
		}
		best = &RateLimitInfo{
			Limit:        limit,
			HasLimit:     hasLimit,
			Remaining:    remaining,
			HasRemaining: hasRemaining,
			Unit:         family.unit,
		}
		bestRank = idx
		bestComplete = complete
		if complete && idx == 0 {
			break
		}
	}

	resetAt, hasReset := firstRateLimitReset(resetFields)
	if best == nil {
		if !hasReset {
			return nil
		}
		return &RateLimitInfo{ResetAt: resetAt}
	}
	if hasReset {
		best.ResetAt = resetAt
	}
	return best
}

func firstRateLimitNumeric(fields map[string]int64, keys []string) (int64, bool) {
	for _, key := range keys {
		if v, ok := fields[key]; ok {
			return v, true
		}
	}
	return 0, false
}

func firstRateLimitReset(fields map[string]time.Time) (time.Time, bool) {
	for _, key := range []string{
		"reset",
		"reset_at",
		"resetat",
		"reset_at_ms",
		"resetatms",
		"reset_time",
		"resettime",
		"reset_timestamp",
		"resettimestamp",
		"next_reset",
		"nextreset",
	} {
		if t, ok := fields[key]; ok && !t.IsZero() {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseNumberAny(raw interface{}) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		return int64(v), true
	case uint8:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > uint64(1<<63-1) {
			return 0, false
		}
		return int64(v), true
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
	case map[string]interface{}, []interface{}:
		return 0, false
	default:
		return parseRateLimitValue(fmt.Sprint(v))
	}
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
	if v, ok := parseNumericToken(raw); ok {
		return v, true
	}
	if token := extractFirstNumberToken(raw); token != "" {
		if v, ok := parseNumericToken(token); ok {
			return v, true
		}
	}
	return 0, false
}

func parseNumericToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	i := 0
	if token[0] == '+' || token[0] == '-' {
		i = 1
	}
	if i >= len(token) {
		return 0, false
	}

	hasDigit := false
	hasDot := false
	for ; i < len(token); i++ {
		c := token[i]
		if isDigit(c) {
			hasDigit = true
			continue
		}
		if c == '.' && !hasDot {
			hasDot = true
			continue
		}
		return 0, false
	}
	if !hasDigit {
		return 0, false
	}

	if hasDot {
		f, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	}

	v, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func extractFirstNumberToken(raw string) string {
	start := -1
	end := -1
	seenDot := false
	seenDigit := false

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if start < 0 {
			if c == '+' || c == '-' {
				if i+1 < len(raw) && (isDigit(raw[i+1]) || raw[i+1] == '.') {
					start = i
					continue
				}
				continue
			}
			if c == '.' {
				if i+1 < len(raw) && isDigit(raw[i+1]) {
					start = i
					seenDot = true
					continue
				}
				continue
			}
			if isDigit(c) {
				start = i
				seenDigit = true
				continue
			}
			continue
		}

		if isDigit(c) {
			seenDigit = true
			end = i + 1
			continue
		}
		if c == '.' && !seenDot {
			seenDot = true
			if end < 0 {
				end = i + 1
			}
			continue
		}
		break
	}

	if start < 0 || !seenDigit {
		return ""
	}
	if end < 0 {
		end = len(raw)
	}
	return raw[start:end]
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func parseRateLimitReset(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
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
	return time.Time{}
}

func encodeJSONBytes(v interface{}) []byte {
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return grokJSONEmptyObjectBytes
	}
	raw := buf.Bytes()
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		return raw[:n-1]
	}
	return raw
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
	for _, msg := range messages {
		roleRaw := strings.TrimSpace(msg.Role)
		role := strings.ToLower(roleRaw)
		if _, ok := allowedMessageRoles[role]; !ok {
			return fmt.Errorf("role must be one of [assistant developer system tool user]")
		}
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(fmt.Sprint(tc.Function["name"])) == "" {
					return fmt.Errorf("assistant tool_calls.function.name cannot be empty")
				}
			}
		}
		if role == "tool" && strings.TrimSpace(msg.ToolCallID) == "" {
			return fmt.Errorf("tool messages must include tool_call_id")
		}
		switch content := msg.Content.(type) {
		case string:
			if strings.TrimSpace(content) == "" && !(role == "assistant" && len(msg.ToolCalls) > 0) {
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
				blockTypeRaw := strings.TrimSpace(fmt.Sprint(rawType))
				blockType := strings.ToLower(blockTypeRaw)
				if blockType == "" {
					return fmt.Errorf("content block 'type' cannot be empty")
				}

				if role == "user" {
					if _, ok := userContentTypes[blockType]; !ok {
						return fmt.Errorf("invalid content block type: '%s'", blockTypeRaw)
					}
				} else if blockType != "text" {
					return fmt.Errorf("the '%s' role only supports 'text' type, got '%s'", role, blockTypeRaw)
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
					return fmt.Errorf("invalid content block type: '%s'", blockTypeRaw)
				}

			}
		default:
			if role == "assistant" && len(msg.ToolCalls) > 0 && msg.Content == nil {
				continue
			}
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

func extractVideoPromptAndAttachments(messages []ChatMessage) (string, []AttachmentInput, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		content := msg.Content
		switch c := content.(type) {
		case string:
			if text := strings.TrimSpace(c); text != "" {
				return text, nil, nil
			}
		case []interface{}:
			textParts := make([]string, 0)
			refs := make([]AttachmentInput, 0)
			for _, block := range c {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						textParts = append(textParts, strings.TrimSpace(s))
					}
				case "image_url":
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						refs = append(refs, AttachmentInput{Type: "image", Data: data})
					}
				case "file":
					return "", nil, fmt.Errorf("video model does not support file content blocks")
				case "input_audio":
					return "", nil, fmt.Errorf("video model does not support input_audio content blocks")
				}
			}
			if len(textParts) > 0 {
				if len(refs) > 7 {
					refs = refs[:7]
				}
				return strings.Join(textParts, " "), refs, nil
			}
		default:
			if text := strings.TrimSpace(extractContentText(content)); text != "" {
				return text, nil, nil
			}
		}
	}
	return "", nil, fmt.Errorf("video prompt cannot be empty")
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
	return util.UniqueStrings(input)
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

func resolveVideoSize(sizeOrRatio string) (aspectRatio string, resolutionName string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(sizeOrRatio))
	if s == "" {
		s = "720x1280"
	}
	switch s {
	case "720x1280", "1024x1792", "9:16":
		return "9:16", "720p", true
	case "1280x720", "1792x1024", "16:9":
		return "16:9", "720p", true
	case "1024x1024", "1:1":
		return "1:1", "720p", true
	case "2:3":
		return "2:3", "720p", true
	case "3:2":
		return "3:2", "720p", true
	default:
		return "", "", false
	}
}

func canonicalVideoSize(sizeOrRatio string) string {
	s := strings.ToLower(strings.TrimSpace(sizeOrRatio))
	switch s {
	case "", "720x1280", "9:16":
		return "720x1280"
	case "1280x720", "16:9":
		return "1280x720"
	case "1024x1024", "1:1":
		return "1024x1024"
	case "1024x1792":
		return "1024x1792"
	case "1792x1024":
		return "1792x1024"
	case "2:3":
		return "1024x1792"
	case "3:2":
		return "1792x1024"
	default:
		return s
	}
}

func validateVideoConfig(cfg *VideoConfig) (*VideoConfig, error) {
	if cfg == nil {
		cfg = &VideoConfig{}
	}
	cfg.Normalize()

	ar := strings.TrimSpace(cfg.AspectRatio)
	if ar == "" && strings.TrimSpace(cfg.Size) != "" {
		var defaultResolution string
		var ok bool
		ar, defaultResolution, ok = resolveVideoSize(cfg.Size)
		if !ok {
			return nil, fmt.Errorf("size must be one of [720x1280 1280x720 1024x1024 1024x1792 1792x1024]")
		}
		if strings.TrimSpace(cfg.ResolutionName) == "" {
			cfg.ResolutionName = defaultResolution
		}
	}
	if ar == "" {
		ar = "720x1280"
	}
	mapped, ok := videoAspectRatioMap[ar]
	if !ok {
		return nil, fmt.Errorf("aspect_ratio must be one of [1280x720 720x1280 1792x1024 1024x1792 1024x1024 16:9 9:16 3:2 2:3 1:1]")
	}
	cfg.AspectRatio = mapped

	if cfg.VideoLength != 6 && cfg.VideoLength != 10 && cfg.VideoLength != 12 && cfg.VideoLength != 16 && cfg.VideoLength != 20 {
		return nil, fmt.Errorf("video_length must be one of [6, 10, 12, 16, 20] seconds")
	}
	resolution := strings.TrimSpace(cfg.ResolutionName)
	if resolution == "" {
		resolution = "720p"
	}
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
	if strings.TrimSpace(cfg.Size) == "" {
		cfg.Size = canonicalVideoSize(ar)
	} else {
		cfg.Size = canonicalVideoSize(cfg.Size)
	}
	return cfg, nil
}

func videoSegmentLengths(seconds int) ([]int, error) {
	switch seconds {
	case 6:
		return []int{6}, nil
	case 10:
		return []int{10}, nil
	case 12:
		return []int{6, 6}, nil
	case 16:
		return []int{10, 6}, nil
	case 20:
		return []int{10, 10}, nil
	default:
		return nil, fmt.Errorf("video_length must be one of [6, 10, 12, 16, 20] seconds")
	}
}

func videoExtensionStartTime(seconds int) float64 {
	return math.Round((float64(seconds)+1.0/24.0)*1e6) / 1e6
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

func normalizeImageEditSize(size string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return "1024x1024", nil
	}
	if s != "1024x1024" {
		return "", fmt.Errorf("image edit currently only supports size '1024x1024'")
	}
	return "1024x1024", nil
}

func extractImageProgress(resp map[string]interface{}) (index int, progress int, ok bool) {
	raw := mapAtAnyPath(resp,
		[]string{"streamingImageGenerationResponse"},
		[]string{"streaming_image_generation_response"},
		[]string{"imageGenerationProgress"},
		[]string{"image_generation_progress"},
		[]string{"modelResponse", "streamingImageGenerationResponse"},
		[]string{"modelResponse", "streaming_image_generation_response"},
	)
	if raw == nil {
		return 0, 0, false
	}
	return intAtAnyPath(raw,
			[]string{"imageIndex"},
			[]string{"image_index"},
			[]string{"index"},
		),
		intAtAnyPath(raw,
			[]string{"progress"},
			[]string{"percent"},
			[]string{"percentage"},
			[]string{"completedPercentage"},
		), true
}

func imageChunkValuesFromAttachment(attachment map[string]interface{}) []map[string]interface{} {
	if attachment == nil {
		return nil
	}
	var values []map[string]interface{}
	appendChunkContainers := func(v interface{}) {
		m, ok := parseGrokJSONData(v).(map[string]interface{})
		if !ok || len(m) == 0 {
			return
		}
		for _, key := range []string{"image_chunk", "imageChunk", "image", "media"} {
			if chunk, ok := m[key].(map[string]interface{}); ok && len(chunk) > 0 {
				values = append(values, chunk)
			}
		}
		values = append(values, m)
	}
	appendChunkContainers(attachment["jsonData"])
	appendChunkContainers(attachment["json"])
	appendChunkContainers(attachment["data"])
	appendChunkContainers(attachment["metadata"])
	return values
}

func appChatImageChunkValues(resp map[string]interface{}) []map[string]interface{} {
	if resp == nil {
		return nil
	}
	var values []map[string]interface{}
	appendIfMap := func(v interface{}) {
		if m, ok := v.(map[string]interface{}); ok && len(m) > 0 {
			values = append(values, m)
		}
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	if mr := extractUpstreamModelResponse(response); mr != nil {
		response = mr
	}
	for _, key := range []string{"streamingImageGenerationResponse", "imageGenerationResponse", "image_chunk", "imageChunk"} {
		appendIfMap(response[key])
	}
	if attachment, ok := response["cardAttachment"].(map[string]interface{}); ok {
		values = append(values, imageChunkValuesFromAttachment(attachment)...)
	}
	if metadata, ok := response["finalMetadata"].(map[string]interface{}); ok {
		for _, key := range []string{"image_chunk", "imageChunk", "streamingImageGenerationResponse", "imageGenerationResponse"} {
			appendIfMap(metadata[key])
		}
	}
	return values
}

func isModeratedImageChunk(chunk map[string]interface{}) bool {
	if chunk == nil {
		return false
	}
	for _, key := range []string{"moderated", "isModerated", "blocked", "isBlocked", "contentFiltered"} {
		if v, ok := chunk[key].(bool); ok && v {
			return true
		}
	}
	status := strings.ToLower(strings.TrimSpace(fmt.Sprint(chunk["status"])))
	return status == "blocked" || status == "moderated"
}

func extractAppChatImageURLs(resp map[string]interface{}) []string {
	var urls []string
	seen := map[string]struct{}{}
	addURL := func(u string) {
		u = strings.TrimSpace(normalizeGrokAssetURL(u))
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		urls = append(urls, u)
	}
	for _, chunk := range appChatImageChunkValues(resp) {
		progress := interfaceToInt(chunk["progress"])
		if progress == 0 {
			progress = interfaceToInt(chunk["percentage"])
		}
		if progress > 0 && progress < 100 {
			continue
		}
		if isModeratedImageChunk(chunk) {
			continue
		}
		for _, key := range []string{"imageUrl", "url", "mediaUrl", "generatedImageUrl", "assetUrl"} {
			if raw := strings.TrimSpace(fmt.Sprint(chunk[key])); raw != "" && raw != "<nil>" {
				addURL(raw)
			}
		}
		firstChunkString := func(keys ...string) string {
			for _, key := range keys {
				raw := strings.TrimSpace(fmt.Sprint(chunk[key]))
				if raw != "" && raw != "<nil>" {
					return raw
				}
			}
			return ""
		}
		assetID := firstChunkString("assetId", "asset_id", "fileId", "file_id")
		userID := firstChunkString("userId", "user_id")
		if assetID != "" && assetID != "<nil>" {
			if userID != "" && userID != "<nil>" {
				addURL("users/" + strings.Trim(userID, "/") + "/" + strings.Trim(assetID, "/") + "/content")
			} else if strings.Contains(assetID, "/") {
				addURL(assetID)
			}
		}
	}
	return urls
}

func appChatImageNoImageDiagnostics(resp map[string]interface{}) []string {
	if resp == nil {
		return nil
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	var out []string
	if msg := firstNonEmptyStringAtAnyPath(response,
		[]string{"userResponse", "message"},
		[]string{"modelResponse", "message"},
		[]string{"message"},
		[]string{"error", "message"},
	); msg != "" {
		out = append(out, "message="+truncateDiagnosticText(msg, 220))
	}
	for _, path := range [][]string{
		{"userResponse", "streamErrors"},
		{"modelResponse", "streamErrors"},
		{"streamErrors"},
		{"errors"},
	} {
		if v := valueAtPath(response, path...); v != nil {
			if s := diagnosticValueSummary(v); s != "" {
				out = append(out, strings.Join(path, ".")+"="+s)
			}
		}
	}
	return out
}

func isAppChatImageLimitResponse(resp map[string]interface{}) bool {
	if resp == nil {
		return false
	}
	response := resp
	if nested := mapAtAnyPath(resp, []string{"result", "response"}); nested != nil {
		response = nested
	}
	for _, path := range [][]string{
		{"error", "renderToolRateLimited"},
		{"userResponse", "streamErrors"},
		{"modelResponse", "streamErrors"},
		{"streamErrors"},
		{"errors"},
	} {
		if strings.Contains(strings.ToLower(diagnosticValueSummary(valueAtPath(response, path...))), "rendertoolratelimited") {
			return true
		}
	}
	for _, msg := range []string{
		firstNonEmptyStringAtAnyPath(response, []string{"error", "message"}),
		firstNonEmptyStringAtAnyPath(response, []string{"modelResponse", "message"}),
	} {
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "image generation limit") || strings.Contains(lower, "try again later") {
			return true
		}
	}
	return false
}

func firstNonEmptyStringAtAnyPath(root interface{}, paths ...[]string) string {
	for _, path := range paths {
		if s := strings.TrimSpace(fmt.Sprint(valueAtPath(root, path...))); s != "" && s != "<nil>" {
			return s
		}
	}
	return ""
}

func diagnosticValueSummary(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return truncateDiagnosticText(x, 220)
	case []interface{}:
		if len(x) == 0 {
			return "[]"
		}
		return fmt.Sprintf("len=%d first=%s", len(x), truncateDiagnosticText(fmt.Sprint(x[0]), 180))
	case map[string]interface{}:
		if len(x) == 0 {
			return "{}"
		}
		b, _ := json.Marshal(x)
		return truncateDiagnosticText(string(b), 220)
	default:
		return truncateDiagnosticText(fmt.Sprint(x), 220)
	}
}

func truncateDiagnosticText(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "..."
}

func extractVideoProgress(resp map[string]interface{}) (progress int, videoURL, thumbnailURL string, ok bool) {
	raw := mapAtAnyPath(resp,
		[]string{"streamingVideoGenerationResponse"},
		[]string{"streaming_video_generation_response"},
		[]string{"videoGenerationProgress"},
		[]string{"video_generation_progress"},
		[]string{"modelResponse", "streamingVideoGenerationResponse"},
		[]string{"modelResponse", "streaming_video_generation_response"},
	)
	if raw == nil {
		return 0, "", "", false
	}
	return intAtAnyPath(raw,
			[]string{"progress"},
			[]string{"percent"},
			[]string{"percentage"},
			[]string{"completedPercentage"},
		),
		stringAtAnyPath(raw,
			[]string{"videoUrl"},
			[]string{"videoURL"},
			[]string{"video_url"},
			[]string{"resultUrl"},
			[]string{"result_url"},
			[]string{"url"},
		),
		stringAtAnyPath(raw,
			[]string{"thumbnailImageUrl"},
			[]string{"thumbnailURL"},
			[]string{"thumbnail_image_url"},
			[]string{"thumbnailUrl"},
			[]string{"posterUrl"},
			[]string{"poster_url"},
		), true
}

func extractVideoPostID(resp map[string]interface{}) string {
	raw := mapAtAnyPath(resp,
		[]string{"streamingVideoGenerationResponse"},
		[]string{"streaming_video_generation_response"},
		[]string{"videoGenerationProgress"},
		[]string{"video_generation_progress"},
		[]string{"modelResponse", "streamingVideoGenerationResponse"},
		[]string{"modelResponse", "streaming_video_generation_response"},
	)
	if raw == nil {
		return ""
	}
	return stringAtAnyPath(raw,
		[]string{"videoPostId"},
		[]string{"video_post_id"},
	)
}

func extractVideoAssetIDs(resp map[string]interface{}) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(v interface{}) {
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" || s == "<nil>" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lk := strings.ToLower(strings.TrimSpace(k))
				switch lk {
				case "assetid", "asset_id":
					add(item)
				case "fileattachments", "file_attachments":
					if arr, ok := item.([]interface{}); ok {
						for _, one := range arr {
							add(one)
						}
						continue
					}
					add(item)
				default:
					walk(item)
				}
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(resp)
	return out
}

func videoURLFromAssetID(assetID string) string {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(assetID), "http://") || strings.HasPrefix(strings.ToLower(assetID), "https://") {
		return assetID
	}
	if strings.Contains(assetID, "/") {
		return defaultAssetsBaseURL + "/" + strings.TrimLeft(assetID, "/")
	}
	return defaultAssetsBaseURL + "/" + assetID + "/content"
}

func imageURLFromAssetID(assetID string) string {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(assetID), "http://") || strings.HasPrefix(strings.ToLower(assetID), "https://") {
		return assetID
	}
	if strings.Contains(assetID, "/") {
		return defaultAssetsBaseURL + "/" + strings.TrimLeft(assetID, "/")
	}
	return defaultAssetsBaseURL + "/" + assetID + "/content"
}

func extractImageAssetIDs(resp map[string]interface{}) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(v interface{}) {
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" || s == "<nil>" {
			return
		}
		if _, exists := seen[s]; exists {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "assetid", "asset_id":
					add(item)
				case "fileattachments", "file_attachments":
					if arr, ok := item.([]interface{}); ok {
						for _, one := range arr {
							add(one)
						}
						continue
					}
					add(item)
				default:
					walk(item)
				}
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(resp)
	return out
}

func appendImageResultURLs(urls []string, resp map[string]interface{}) []string {
	added := make(map[string]struct{}, len(urls)+8)
	for _, u := range urls {
		added[u] = struct{}{}
	}
	addURL := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if _, exists := added[u]; exists {
			return
		}
		added[u] = struct{}{}
		urls = append(urls, u)
	}
	addURLs := func(items []string) {
		for _, u := range items {
			addURL(u)
		}
	}
	if mr := extractUpstreamModelResponse(resp); mr != nil {
		addURLs(extractImageURLs(mr))
		for _, assetID := range extractImageAssetIDs(mr) {
			if u := imageURLFromAssetID(assetID); u != "" {
				addURL(u)
			}
		}
	}
	addURLs(extractImageURLs(resp))
	for _, assetID := range extractImageAssetIDs(resp) {
		if u := imageURLFromAssetID(assetID); u != "" {
			addURL(u)
		}
	}
	return urls
}

func imageDebugShape(resp map[string]interface{}) string {
	if resp == nil {
		return "nil"
	}
	var parts []string
	var walk func(interface{}, string, int)
	walk = func(v interface{}, prefix string, depth int) {
		if depth > 4 || len(parts) >= 40 {
			return
		}
		switch x := v.(type) {
		case map[string]interface{}:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if prefix != "" {
				parts = append(parts, prefix+"{"+strings.Join(keys, ",")+"}")
			} else {
				parts = append(parts, "{"+strings.Join(keys, ",")+"}")
			}
			for _, k := range keys {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "image") ||
					strings.Contains(lk, "card") ||
					strings.Contains(lk, "error") ||
					strings.Contains(lk, "message") ||
					strings.Contains(lk, "moderation") ||
					strings.Contains(lk, "progress") ||
					strings.Contains(lk, "response") ||
					strings.Contains(lk, "result") {
					next := k
					if prefix != "" {
						next = prefix + "." + k
					}
					walk(x[k], next, depth+1)
				}
			}
		case []interface{}:
			parts = append(parts, prefix+"[]")
			if len(x) > 0 {
				walk(x[0], prefix+"[]", depth+1)
			}
		case string:
			if prefix != "" {
				parts = append(parts, prefix+"=string")
			}
		case float64, bool, nil:
			if prefix != "" {
				parts = append(parts, prefix+"="+fmt.Sprintf("%T", x))
			}
		}
	}
	walk(resp, "", 0)
	return strings.Join(parts, " ")
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

func interfaceSlice(v interface{}) []interface{} {
	switch x := v.(type) {
	case []interface{}:
		return x
	default:
		return nil
	}
}

func valueAtPath(root interface{}, path ...string) interface{} {
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur, ok = m[key]
		if !ok {
			return nil
		}
	}
	return cur
}

func mapAtAnyPath(root interface{}, paths ...[]string) map[string]interface{} {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		if m, ok := valueAtPath(root, path...).(map[string]interface{}); ok && len(m) > 0 {
			return m
		}
	}
	return nil
}

func stringAtAnyPath(root interface{}, paths ...[]string) string {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		v := strings.TrimSpace(fmt.Sprint(valueAtPath(root, path...)))
		if v != "" && v != "<nil>" {
			return v
		}
	}
	return ""
}

func intAtAnyPath(root interface{}, paths ...[]string) int {
	for _, path := range paths {
		if len(path) == 0 {
			continue
		}
		if v := interfaceToInt(valueAtPath(root, path...)); v != 0 {
			return v
		}
	}
	return 0
}

func extractUpstreamModelResponse(resp map[string]interface{}) map[string]interface{} {
	return mapAtAnyPath(resp,
		[]string{"modelResponse"},
		[]string{"model_response"},
		[]string{"userResponse"},
		[]string{"user_response"},
		[]string{"messageResponse"},
		[]string{"message_response"},
		[]string{"output"},
	)
}

func extractUpstreamFingerprint(resp map[string]interface{}, modelResp map[string]interface{}) string {
	if fp := stringAtAnyPath(resp,
		[]string{"llmInfo", "modelHash"},
		[]string{"llm_info", "modelHash"},
		[]string{"llm_info", "model_hash"},
		[]string{"metadata", "llm_info", "modelHash"},
		[]string{"metadata", "llm_info", "model_hash"},
	); fp != "" {
		return fp
	}
	return stringAtAnyPath(modelResp,
		[]string{"metadata", "llm_info", "modelHash"},
		[]string{"metadata", "llm_info", "model_hash"},
		[]string{"metadata", "llmInfo", "modelHash"},
		[]string{"llmInfo", "modelHash"},
	)
}

func extractUpstreamResponseID(resp map[string]interface{}, modelResp map[string]interface{}) string {
	if rid := stringAtAnyPath(resp,
		[]string{"responseId"},
		[]string{"response_id"},
		[]string{"id"},
		[]string{"messageId"},
		[]string{"message_id"},
	); rid != "" {
		return rid
	}
	return stringAtAnyPath(modelResp,
		[]string{"responseId"},
		[]string{"response_id"},
		[]string{"id"},
		[]string{"messageId"},
		[]string{"message_id"},
	)
}

func extractUpstreamMessage(modelResp map[string]interface{}) string {
	if modelResp == nil {
		return ""
	}
	if msg := stringAtAnyPath(modelResp,
		[]string{"message"},
		[]string{"content"},
		[]string{"text"},
		[]string{"outputText"},
		[]string{"output_text"},
	); msg != "" {
		return msg
	}
	if raw := mapAtAnyPath(modelResp, []string{"message"}, []string{"content"}, []string{"text"}); raw != nil {
		return stringAtAnyPath(raw,
			[]string{"text"},
			[]string{"content"},
			[]string{"value"},
			[]string{"body"},
		)
	}
	return ""
}

func extractUpstreamTokenDelta(resp map[string]interface{}, modelResp map[string]interface{}) string {
	if token := stringAtAnyPath(resp,
		[]string{"token"},
		[]string{"delta"},
		[]string{"text"},
		[]string{"contentDelta"},
		[]string{"content_delta"},
		[]string{"messageDelta"},
		[]string{"message_delta"},
	); token != "" {
		return token
	}
	return stringAtAnyPath(modelResp,
		[]string{"token"},
		[]string{"delta"},
		[]string{"textDelta"},
		[]string{"text_delta"},
	)
}
