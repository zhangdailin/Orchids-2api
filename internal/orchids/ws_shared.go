package orchids

import (
	"crypto/rand"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"orchids-api/internal/clerk"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/util"
)

const (
	orchidsWSConnectTimeout = 5 * time.Second // Reduced from 10s for faster retry
	orchidsWSReadTimeout    = 600 * time.Second
	orchidsWSRequestTimeout = 60 * time.Second
	orchidsWSPingInterval   = 10 * time.Second
	orchidsWSUserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36"
	orchidsWSOrigin         = "https://www.orchids.app"
)

var (
	promptToolOrder = []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "TodoWrite"}
	jwtLikePattern  = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

type orchidsToolSpec struct {
	ToolSpecification struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		InputSchema map[string]interface{} `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type wsFallbackError struct {
	err error
}

func (e wsFallbackError) Error() string {
	return e.err.Error()
}

func (e wsFallbackError) Unwrap() error {
	return e.err
}

func SupportedToolNames(tools []interface{}) []string {
	if len(tools) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(promptToolOrder))
	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		if name == "" {
			continue
		}
		mappedName := DefaultToolMapper.ToOrchids(name)
		if !isOrchidsToolSupported(mappedName) {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(mappedName))] = struct{}{}
	}

	if len(seen) == 0 {
		return nil
	}

	out := make([]string, 0, len(seen))
	for _, name := range promptToolOrder {
		if _, ok := seen[strings.ToLower(name)]; ok {
			out = append(out, name)
		}
	}
	return out
}

func (c *Client) getWSToken() (string, error) {
	if c.config != nil && strings.TrimSpace(c.config.UpstreamToken) != "" {
		return c.config.UpstreamToken, nil
	}

	if c.config != nil && strings.TrimSpace(c.config.ClientCookie) != "" {
		proxyFunc := http.ProxyFromEnvironment
		if c.config != nil {
			proxyFunc = util.ProxyFunc(c.config.ProxyHTTP, c.config.ProxyHTTPS, c.config.ProxyUser, c.config.ProxyPass, c.config.ProxyBypass)
		}
		info, err := clerk.FetchAccountInfoWithProjectAndSessionProxy(c.config.ClientCookie, c.config.SessionCookie, c.config.ProjectID, proxyFunc)
		if err == nil && info.JWT != "" {
			return info.JWT, nil
		}
	}

	return c.GetToken()
}

const (
	maxCompactToolCount         = 24
	maxCompactToolDescLen       = 512
	maxCompactToolSchemaJSONLen = 4096
	maxOrchidsToolCount         = 12
	maxIncomingToolDescLen      = 128
)

var incomingToolPropertyAllowlist = map[string]map[string]struct{}{
	"bash": {
		"command":                   {},
		"description":               {},
		"dangerouslyDisableSandbox": {},
		"run_in_background":         {},
		"timeout":                   {},
	},
	"glob": {
		"path":    {},
		"pattern": {},
	},
	"grep": {
		"-A":          {},
		"-B":          {},
		"-C":          {},
		"-i":          {},
		"-n":          {},
		"context":     {},
		"glob":        {},
		"head_limit":  {},
		"multiline":   {},
		"offset":      {},
		"output_mode": {},
		"path":        {},
		"pattern":     {},
		"type":        {},
	},
	"read": {
		"file_path": {},
		"limit":     {},
		"offset":    {},
		"pages":     {},
	},
	"edit": {
		"file_path":   {},
		"new_string":  {},
		"old_string":  {},
		"replace_all": {},
	},
	"write": {
		"content":   {},
		"file_path": {},
	},
}

func convertOrchidsTools(tools []interface{}) []orchidsToolSpec {
	if len(tools) == 0 {
		return nil
	}

	var out []orchidsToolSpec
	seen := make(map[string]struct{})
	for _, tool := range tools {
		name, description, inputSchema := extractToolSpecFields(tool)
		if name == "" {
			continue
		}

		mappedName := DefaultToolMapper.ToOrchids(name)
		if !isOrchidsToolSupported(mappedName) {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(mappedName))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		description = compactToolDescription(description)
		inputSchema = compactToolSchema(inputSchema)
		if inputSchema == nil {
			inputSchema = map[string]interface{}{}
		}

		var spec orchidsToolSpec
		spec.ToolSpecification.Name = mappedName
		spec.ToolSpecification.Description = description
		spec.ToolSpecification.InputSchema = map[string]interface{}{
			"json": inputSchema,
		}
		out = append(out, spec)
		if len(out) >= maxOrchidsToolCount {
			break
		}
	}
	return out
}

// compactIncomingTools reduces tool definition size for SSE mode while preserving original tool shape.
func compactIncomingTools(tools []interface{}) []interface{} {
	if len(tools) == 0 {
		return nil
	}

	out := make([]interface{}, 0, len(tools))
	seen := make(map[string]struct{})

	for _, raw := range tools {
		rawMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		name, description, schema := extractToolSpecFields(rawMap)
		if name == "" {
			continue
		}

		mappedName := DefaultToolMapper.ToOrchids(name)
		if !isOrchidsToolSupported(mappedName) {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(mappedName))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		description = compactIncomingToolDescription(description)
		schema = compactIncomingToolSchema(mappedName, schema)

		rebuilt := map[string]interface{}{}
		if fn, ok := rawMap["function"].(map[string]interface{}); ok {
			_ = fn
			rebuilt["type"] = "function"
			function := map[string]interface{}{
				"name": strings.TrimSpace(name),
			}
			if description != "" {
				function["description"] = description
			}
			if len(schema) > 0 {
				function["parameters"] = schema
			}
			rebuilt["function"] = function
		} else {
			rebuilt["name"] = strings.TrimSpace(name)
			if description != "" {
				rebuilt["description"] = description
			}
			if len(schema) > 0 {
				rebuilt["input_schema"] = schema
			}
		}

		out = append(out, rebuilt)
		if len(out) >= maxCompactToolCount {
			break
		}
	}
	return out
}

func compactIncomingToolDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	runes := []rune(description)
	if len(runes) <= maxIncomingToolDescLen {
		return description
	}
	return string(runes[:maxIncomingToolDescLen]) + "...[truncated]"
}

func compactIncomingToolSchema(toolName string, schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	cleaned := cleanJSONSchemaProperties(schema)
	if cleaned == nil {
		return nil
	}
	stripped := stripSchemaDescriptions(cleaned)
	filtered := filterIncomingToolSchema(toolName, stripped)
	if schemaJSONLen(filtered) <= maxCompactToolSchemaJSONLen {
		return filtered
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func filterIncomingToolSchema(toolName string, schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	allowlist, ok := incomingToolPropertyAllowlist[strings.ToLower(strings.TrimSpace(toolName))]
	if !ok || len(allowlist) == 0 {
		return schema
	}

	filtered := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		switch key {
		case "properties":
			props, _ := value.(map[string]interface{})
			if len(props) == 0 {
				continue
			}
			nextProps := make(map[string]interface{}, len(props))
			for propName, propValue := range props {
				if _, keep := allowlist[propName]; !keep {
					continue
				}
				nextProps[propName] = propValue
			}
			if len(nextProps) > 0 {
				filtered["properties"] = nextProps
			}
		case "required":
			switch required := value.(type) {
			case []interface{}:
				if len(required) == 0 {
					continue
				}
				nextRequired := make([]interface{}, 0, len(required))
				for _, item := range required {
					name, _ := item.(string)
					if _, keep := allowlist[name]; keep {
						nextRequired = append(nextRequired, item)
					}
				}
				if len(nextRequired) > 0 {
					filtered["required"] = nextRequired
				}
			case []string:
				if len(required) == 0 {
					continue
				}
				nextRequired := make([]string, 0, len(required))
				for _, name := range required {
					if _, keep := allowlist[name]; keep {
						nextRequired = append(nextRequired, name)
					}
				}
				if len(nextRequired) > 0 {
					filtered["required"] = nextRequired
				}
			}
		default:
			filtered[key] = value
		}
	}
	return filtered
}

func EstimateCompactedToolsTokens(tools []interface{}) int {
	if len(tools) == 0 {
		return 0
	}
	compacted := compactIncomingTools(tools)
	if len(compacted) == 0 {
		return 0
	}
	raw, err := json.Marshal(compacted)
	if err != nil {
		return 0
	}
	var estimator tiktoken.Estimator
	estimator.AddBytes(raw)
	return estimator.Count()
}

func compactToolDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	runes := []rune(description)
	if len(runes) <= maxCompactToolDescLen {
		return description
	}
	return string(runes[:maxCompactToolDescLen]) + "...[truncated]"
}

func compactToolSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	cleaned := cleanJSONSchemaProperties(schema)
	if cleaned == nil {
		return nil
	}
	if schemaJSONLen(cleaned) <= maxCompactToolSchemaJSONLen {
		return cleaned
	}
	stripped := stripSchemaDescriptions(cleaned)
	if schemaJSONLen(stripped) <= maxCompactToolSchemaJSONLen {
		return stripped
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func stripSchemaDescriptions(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	out := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		if strings.EqualFold(k, "description") || strings.EqualFold(k, "title") {
			continue
		}
		if strings.EqualFold(k, "properties") {
			if props, ok := v.(map[string]interface{}); ok {
				cleanProps := make(map[string]interface{}, len(props))
				for name, prop := range props {
					cleanProps[name] = stripSchemaDescriptionsValue(prop)
				}
				out[k] = cleanProps
				continue
			}
		}
		out[k] = stripSchemaDescriptionsValue(v)
	}
	return out
}

func stripSchemaDescriptionsValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return stripSchemaDescriptions(v)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, stripSchemaDescriptionsValue(item))
		}
		return out
	default:
		return value
	}
}

func schemaJSONLen(schema map[string]interface{}) int {
	if schema == nil {
		return 0
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return 0
	}
	return len(raw)
}

// extractToolSpecFields 支持 Claude/OpenAI 风格的工具定义字段提取
// 兼容：{name, description, input_schema} 与 {type:"function", function:{name, description, parameters}}
func extractToolSpecFields(tool interface{}) (string, string, map[string]interface{}) {
	tm, ok := tool.(map[string]interface{})
	if !ok {
		return "", "", nil
	}
	var name string
	var description string
	var schema map[string]interface{}

	if fn, ok := tm["function"].(map[string]interface{}); ok {
		if v, ok := fn["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
		if v, ok := fn["description"].(string); ok {
			description = v
		}
		schema = extractSchemaMap(fn, "parameters", "input_schema", "inputSchema")
	}
	if name == "" {
		if v, ok := tm["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
	}
	if description == "" {
		if v, ok := tm["description"].(string); ok {
			description = v
		}
	}
	if schema == nil {
		schema = extractSchemaMap(tm, "input_schema", "inputSchema", "parameters")
	}
	return name, description, schema
}

func extractSchemaMap(tm map[string]interface{}, keys ...string) map[string]interface{} {
	if tm == nil {
		return nil
	}
	for _, key := range keys {
		if v, ok := tm[key]; ok {
			if schema, ok := v.(map[string]interface{}); ok {
				return schema
			}
		}
	}
	return nil
}

// cleanJSONSchemaProperties 递归清理不受支持的 JSON Schema 字段
// 仅保留 type/description/properties/required/enum/items，避免上游报错
func cleanJSONSchemaProperties(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	sanitized := map[string]interface{}{}
	for _, key := range []string{"type", "description", "properties", "required", "enum", "items"} {
		if v, ok := schema[key]; ok {
			sanitized[key] = v
		}
	}
	if props, ok := sanitized["properties"].(map[string]interface{}); ok {
		cleanProps := map[string]interface{}{}
		for name, prop := range props {
			cleanProps[name] = cleanJSONSchemaValue(prop)
		}
		sanitized["properties"] = cleanProps
	}
	if items, ok := sanitized["items"]; ok {
		sanitized["items"] = cleanJSONSchemaValue(items)
	}
	return sanitized
}

func cleanJSONSchemaValue(value interface{}) interface{} {
	if value == nil {
		return value
	}
	if m, ok := value.(map[string]interface{}); ok {
		return cleanJSONSchemaProperties(m)
	}
	if arr, ok := value.([]interface{}); ok {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			out = append(out, cleanJSONSchemaValue(item))
		}
		return out
	}
	return value
}

func isOrchidsToolSupported(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read", "write", "edit", "bash", "glob", "grep", "todowrite":
		return true
	default:
		return false
	}
}

func extractOrchidsText(msg map[string]interface{}) string {
	if delta, ok := msg["delta"].(string); ok {
		return delta
	}
	if text, ok := msg["text"].(string); ok {
		return text
	}
	if data, ok := msg["data"].(map[string]interface{}); ok {
		if text, ok := data["text"].(string); ok {
			return text
		}
	}
	if chunk, ok := msg["chunk"]; ok {
		if s, ok := chunk.(string); ok {
			return s
		}
		if m, ok := chunk.(map[string]interface{}); ok {
			if text, ok := m["text"].(string); ok {
				return text
			}
			if text, ok := m["content"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func randomSuffix(length int) string {
	if length <= 0 {
		return "0"
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

func urlEncode(value string) string {
	return url.QueryEscape(value)
}

func formatToolResultContentLocal(content interface{}) string {
	return formatToolResultContentLocalWithMode(content, false)
}

func formatToolResultContentLocalForHistory(content interface{}) string {
	return formatToolResultContentLocalWithMode(content, true)
}

func formatToolResultContentLocalWithMode(content interface{}, historyMode bool) string {
	raw := util.NormalizePersistedToolResultText(extractToolResultText(content))
	raw = redactSensitiveToolResultText(raw)
	return compactLocalToolResultText(raw, historyMode)
}

func redactSensitiveToolResultText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return jwtLikePattern.ReplaceAllString(text, "[redacted_jwt]")
}

func extractToolResultText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []interface{}:
		var parts []string
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if text, ok := itemMap["text"].(string); ok {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func compactLocalToolResultText(text string, historyMode bool) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	if looksLikeDirectoryListing(text) {
		return compactDirectoryListingResult(text, historyMode)
	}
	if historyMode {
		return compactHistoricalToolResultText(text)
	}
	return text
}

func looksLikeDirectoryListing(text string) bool {
	lines := nonEmptyTrimmedLines(text)
	if len(lines) < 4 {
		return false
	}
	entryLike := 0
	strongSignals := 0
	for _, line := range lines {
		if looksLikePathLine(line) {
			entryLike++
			strongSignals++
			continue
		}
		if looksLikeBareDirectoryEntryLine(line) {
			entryLike++
			if hasDirectoryEntrySignal(line) {
				strongSignals++
			}
		}
	}
	return entryLike*100/len(lines) >= 70 && strongSignals > 0
}

func isDirectoryListingLikeText(text string) bool {
	if looksLikeDirectoryListing(text) {
		return true
	}
	return strings.Contains(text, "[directory listing")
}

func looksLikePathLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "/") || strings.HasPrefix(line, "./") || strings.HasPrefix(line, "../") {
		return true
	}
	return len(line) >= 3 && ((line[1] == ':' && line[2] == '\\') || (line[1] == ':' && line[2] == '/'))
}

func looksLikeBareDirectoryEntryLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.ContainsAny(line, "\r\n\t") {
		return false
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "results are truncated") {
		return false
	}
	if strings.HasPrefix(line, "[") || strings.HasPrefix(line, "<") {
		return false
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "• ") {
		return false
	}
	if strings.Contains(line, ": ") || strings.ContainsAny(line, "{}<>|`") {
		return false
	}
	if strings.HasSuffix(line, ".") || strings.HasSuffix(line, "。") || strings.HasSuffix(line, ":") {
		return false
	}
	if strings.Count(line, " ") > 2 {
		return false
	}
	return hasDirectoryEntrySignal(line)
}

func hasDirectoryEntrySignal(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, ".") {
		return true
	}
	return strings.ContainsAny(line, "._-/\\")
}

func compactDirectoryListingResult(text string, historyMode bool) string {
	lines := nonEmptyTrimmedLines(text)
	if len(lines) == 0 {
		return ""
	}
	total := len(lines)
	pathLines, nonPathCount := splitDirectoryListingLines(lines)
	prefix := sharedDirectoryPrefix(pathLines)

	filtered := make([]string, 0, len(pathLines))
	omittedGit := 0
	for _, line := range pathLines {
		if shouldDropDirectoryListingLine(line) {
			omittedGit++
			continue
		}
		filtered = append(filtered, shortenDirectoryListingLine(line, prefix))
	}

	if len(filtered) == 0 {
		return fmt.Sprintf("[directory listing trimmed: omitted %d git metadata entries and %d non-path lines]", total, nonPathCount)
	}

	if shouldSummarizeDirectoryTopLevel(filtered) {
		summarized, summarizedRoots, omittedRootEntries := summarizeDirectoryListingTopLevel(filtered, historyMode)
		if omittedGit > 0 || nonPathCount > 0 || omittedRootEntries > 0 {
			summarized = append(summarized, fmt.Sprintf("[directory listing summarized: %d root entries from %d lines; omitted %d git metadata entries, %d non-path lines, and %d additional root entries]", summarizedRoots, total, omittedGit, nonPathCount, omittedRootEntries))
		}
		result := strings.Join(summarized, "\n")
		if historyMode {
			return truncateTextWithEllipsis(result, 700)
		}
		return truncateTextWithEllipsis(result, 2200)
	}

	limit := 40
	if historyMode {
		limit = 12
	}
	extra := 0
	keptBeforeSummary := len(filtered)
	if keptBeforeSummary > limit {
		extra = keptBeforeSummary - limit
		filtered = filtered[:limit]
	}
	if omittedGit > 0 || nonPathCount > 0 || extra > 0 {
		filtered = append(filtered, fmt.Sprintf("[directory listing trimmed: kept %d of %d entries; omitted %d git metadata entries, %d non-path lines, and %d extra entries]", keptBeforeSummary-extra, total, omittedGit, nonPathCount, extra))
	}

	result := strings.Join(filtered, "\n")
	if historyMode {
		return truncateTextWithEllipsis(result, 700)
	}
	return truncateTextWithEllipsis(result, 2200)
}

func shouldDropDirectoryListingLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.Contains(line, "/.git/") || strings.HasSuffix(line, "/.git") {
		return true
	}
	base := line
	if idx := strings.LastIndexAny(base, `/\`); idx >= 0 {
		base = base[idx+1:]
	}
	switch base {
	case ".DS_Store", "Thumbs.db", "desktop.ini":
		return true
	default:
		return false
	}
}

func splitDirectoryListingLines(lines []string) ([]string, int) {
	pathLines := make([]string, 0, len(lines))
	nonPathCount := 0
	for _, line := range lines {
		if looksLikePathLine(line) || looksLikeBareDirectoryEntryLine(line) {
			pathLines = append(pathLines, line)
			continue
		}
		nonPathCount++
	}
	return pathLines, nonPathCount
}

func shouldSummarizeDirectoryTopLevel(lines []string) bool {
	if len(lines) <= 12 {
		return false
	}
	nested := 0
	for _, line := range lines {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/") {
			nested++
		}
	}
	return nested*100/len(lines) >= 60
}

func summarizeDirectoryListingTopLevel(lines []string, historyMode bool) ([]string, int, int) {
	type rootSummary struct {
		label   string
		samples []string
	}

	maxRoots := 10
	maxSamples := 2
	if historyMode {
		maxRoots = 6
		maxSamples = 1
	}

	order := make([]string, 0, len(lines))
	roots := make(map[string]*rootSummary)

	for _, line := range lines {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if trimmed == "" {
			continue
		}
		parts := strings.Split(trimmed, "/")
		if len(parts) == 0 {
			continue
		}
		key := "./" + parts[0]
		sample := ""
		if len(parts) > 1 {
			key += "/"
			sample = strings.Join(parts[1:], "/")
		}
		summary, ok := roots[key]
		if !ok {
			summary = &rootSummary{label: key}
			roots[key] = summary
			order = append(order, key)
		}
		if sample != "" && len(summary.samples) < maxSamples && !containsString(summary.samples, sample) {
			summary.samples = append(summary.samples, sample)
		}
	}

	out := make([]string, 0, minInt(len(order), maxRoots))
	omitted := 0
	for idx, key := range order {
		if idx >= maxRoots {
			omitted++
			continue
		}
		summary := roots[key]
		if len(summary.samples) == 0 {
			out = append(out, summary.label)
			continue
		}
		out = append(out, fmt.Sprintf("%s (sample: %s)", summary.label, strings.Join(summary.samples, ", ")))
	}
	return out, minInt(len(order), maxRoots), omitted
}

func shortenDirectoryListingLine(line string, prefix string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if prefix != "" && strings.HasPrefix(line, prefix) {
		trimmed := strings.TrimPrefix(line, prefix)
		if trimmed == "" {
			return "./"
		}
		return "./" + trimmed
	}
	return line
}

func sharedDirectoryPrefix(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	pathLines, _ := splitDirectoryListingLines(lines)
	if len(pathLines) == 0 {
		return ""
	}
	prefix := strings.TrimSpace(pathLines[0])
	for _, raw := range pathLines[1:] {
		line := strings.TrimSpace(raw)
		for prefix != "" && !strings.HasPrefix(line, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		if prefix == "" {
			return ""
		}
	}
	idx := strings.LastIndex(prefix, "/")
	if idx < 0 {
		return ""
	}
	return prefix[:idx+1]
}

func compactHistoricalToolResultText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	lines := nonEmptyTrimmedLines(text)
	if len(lines) == 0 {
		return truncateTextWithEllipsis(text, 900)
	}
	if len(lines) > 12 {
		head := lines[:8]
		tail := lines[len(lines)-3:]
		compacted := append([]string{}, head...)
		compacted = append(compacted, fmt.Sprintf("[tool_result summary: omitted %d middle lines]", len(lines)-11))
		compacted = append(compacted, tail...)
		return truncateTextWithEllipsis(strings.Join(compacted, "\n"), 900)
	}
	return truncateTextWithEllipsis(strings.Join(lines, "\n"), 900)
}

func nonEmptyTrimmedLines(text string) []string {
	rawLines := strings.Split(text, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, raw := range rawLines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func truncateTextWithEllipsis(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(text) <= maxLen {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "…[truncated]"
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
