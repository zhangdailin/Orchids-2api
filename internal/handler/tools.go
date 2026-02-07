package handler

import (
	"encoding/json"
	"strings"

	"orchids-api/internal/orchids"
	"orchids-api/internal/perf"
)

// fixToolInput 修复工具输入中的类型问题
func fixToolInput(inputJSON string) string {
	if inputJSON == "" {
		return "{}"
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return inputJSON
	}

	fixed := false
	for key, value := range input {
		if strVal, ok := value.(string); ok {
			if !shouldParseToolInputKey(key) {
				continue
			}
			strVal = strings.TrimSpace(strVal)

			// Only try to fix JSON arrays/objects that were passed as strings.
			// Do NOT auto-convert "true", "123", etc. as this breaks string arguments.
			if (strings.HasPrefix(strVal, "[") && strings.HasSuffix(strVal, "]")) ||
				(strings.HasPrefix(strVal, "{") && strings.HasSuffix(strVal, "}")) {
				var parsed interface{}
				if err := json.Unmarshal([]byte(strVal), &parsed); err == nil {
					input[key] = parsed
					fixed = true
				}
			}
		}
	}

	if !fixed {
		return inputJSON
	}

	result, err := json.Marshal(input)
	if err != nil {
		return inputJSON
	}
	return string(result)
}

// fixToolInputForName 为特定工具补齐必要字段，避免客户端校验报错。
func fixToolInputForName(toolName, inputJSON string) string {
	fixed := fixToolInput(inputJSON)
	if strings.TrimSpace(fixed) == "" {
		fixed = "{}"
	}
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "glob":
		return ensureGlobPattern(fixed)
	default:
		return fixed
	}
}

func ensureGlobPattern(inputJSON string) string {
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return inputJSON
	}

	pattern := firstNonEmptyString(input, "pattern")
	if pattern == "" {
		pattern = firstNonEmptyString(input, "query", "search", "glob")
	}
	if pattern == "" {
		pattern = "*"
	}
	input["pattern"] = pattern

	path := firstNonEmptyString(input, "path", "root", "dir", "file_path", "filePath", "file_paths", "paths", "roots")
	if path == "" {
		path = "."
	}
	input["path"] = path

	result, err := json.Marshal(input)
	if err != nil {
		return inputJSON
	}
	return string(result)
}

func firstNonEmptyString(input map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := input[key]; ok {
			if strVal, ok := val.(string); ok {
				strVal = strings.TrimSpace(strVal)
				if strVal != "" {
					return strVal
				}
			}
		}
	}
	return ""
}

func shouldParseToolInputKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "edits",
		"files",
		"file_paths",
		"filepaths",
		"paths",
		"roots",
		"globparameters",
		"glob_parameters",
		"ripgrepparameters",
		"ripgrep_parameters":
		return true
	default:
		return false
	}
}

type toolNameInfo struct {
	name    string
	lowered string
	short   string
	props   map[string]struct{}
}

func toolNameFromDef(tm map[string]interface{}) string {
	if tm == nil {
		return ""
	}
	if name, ok := tm["name"].(string); ok {
		return strings.TrimSpace(name)
	}
	if fn, ok := tm["function"].(map[string]interface{}); ok {
		if name, ok := fn["name"].(string); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

func normalizeToolName(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	lowered := strings.ToLower(raw)
	short := lowered
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '.' || r == '/' || r == ':'
	})
	if len(parts) > 0 {
		short = strings.ToLower(parts[len(parts)-1])
	}
	return lowered, short
}

func buildToolNameIndex(tools []interface{}, allowed map[string]string) []toolNameInfo {
	if len(tools) == 0 {
		return nil
	}
	index := make([]toolNameInfo, 0, len(tools))
	for _, tool := range tools {
		tm, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		name := toolNameFromDef(tm)
		if name == "" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[strings.ToLower(name)]; !ok {
				continue
			}
		}
		lowered, short := normalizeToolName(name)
		if lowered == "" {
			continue
		}
		props := map[string]struct{}{}
		if schema := toolInputSchemaFromDef(tm); schema != nil {
			if properties, ok := schema["properties"].(map[string]interface{}); ok {
				for key := range properties {
					props[key] = struct{}{}
				}
			}
		}
		index = append(index, toolNameInfo{
			name:    name,
			lowered: lowered,
			short:   short,
			props:   props,
		})
	}
	return index
}

// toolInputSchemaFromDef 兼容 Claude/OpenAI 风格的工具 schema 字段
func toolInputSchemaFromDef(tm map[string]interface{}) map[string]interface{} {
	if tm == nil {
		return nil
	}
	if schema, ok := tm["input_schema"].(map[string]interface{}); ok {
		return schema
	}
	if schema, ok := tm["inputSchema"].(map[string]interface{}); ok {
		return schema
	}
	if schema, ok := tm["parameters"].(map[string]interface{}); ok {
		return schema
	}
	if fn, ok := tm["function"].(map[string]interface{}); ok {
		if schema, ok := fn["parameters"].(map[string]interface{}); ok {
			return schema
		}
		if schema, ok := fn["input_schema"].(map[string]interface{}); ok {
			return schema
		}
		if schema, ok := fn["inputSchema"].(map[string]interface{}); ok {
			return schema
		}
	}
	return nil
}

func mapOrchidsToolName(raw string, inputStr string, index []toolNameInfo, allowed map[string]string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(allowed) == 0 {
		return raw
	}
	lowered, short := normalizeToolName(raw)
	if lowered == "" {
		return raw
	}
	if resolved, ok := allowed[lowered]; ok {
		return resolved
	}
	for _, info := range index {
		if info.short == short {
			return info.name
		}
	}

	mapped := orchids.NormalizeToolName(raw)
	if resolved, ok := allowed[strings.ToLower(mapped)]; ok {
		return resolved
	}

	for _, info := range index {
		if info.short == short {
			return info.name
		}
	}

	for _, info := range index {
		if info.short == strings.ToLower(mapped) {
			return info.name
		}
	}

	inputKeys := parseToolInputKeys(inputStr)
	if len(inputKeys) > 0 {
		var bestName string
		bestScore := -1
		bestExtra := 0
		for _, info := range index {
			if len(info.props) == 0 {
				continue
			}
			score := 0
			for _, key := range inputKeys {
				if _, ok := info.props[key]; ok {
					score++
				} else {
					score = -1
					break
				}
			}
			if score < 0 {
				continue
			}
			extra := len(info.props) - score
			if score > bestScore || (score == bestScore && extra < bestExtra) {
				bestScore = score
				bestExtra = extra
				bestName = info.name
			}
		}
		if bestName != "" {
			return bestName
		}
	}
	return raw
}

func normalizeWarpTools(tools []interface{}) []interface{} {
	if len(tools) == 0 {
		return tools
	}
	out := make([]interface{}, 0, len(tools))
	for _, raw := range tools {
		tm, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		copied := make(map[string]interface{}, len(tm))
		for k, v := range tm {
			copied[k] = v
		}
		if typ, _ := copied["type"].(string); typ == "function" {
			if fn, ok := copied["function"].(map[string]interface{}); ok {
				fnCopy := make(map[string]interface{}, len(fn))
				for k, v := range fn {
					fnCopy[k] = v
				}
				if name, ok := fnCopy["name"].(string); ok {
					if orchids.DefaultToolMapper.IsBlocked(name) {
						continue
					}
					fnCopy["name"] = orchids.NormalizeToolName(name)
				}
				copied["function"] = fnCopy
				out = append(out, copied)
				continue
			}
		}
		if name, ok := copied["name"].(string); ok {
			if orchids.DefaultToolMapper.IsBlocked(name) {
				continue
			}
			copied["name"] = orchids.NormalizeToolName(name)
		}
		out = append(out, copied)
	}
	return out
}

func parseToolInputKeys(inputStr string) []string {
	inputStr = strings.TrimSpace(inputStr)
	if inputStr == "" {
		return nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(inputStr), &payload); err != nil {
		return nil
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	return keys
}

func injectToolGate(promptText string, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return promptText
	}
	section := "<tool_gate>\n" + message + "\n</tool_gate>\n\n"
	_, idx := findUserMarker(promptText)

	sb := perf.AcquireStringBuilder()
	defer perf.ReleaseStringBuilder(sb)

	if idx != -1 {
		sb.Grow(len(promptText) + len(section))
		sb.WriteString(promptText[:idx])
		sb.WriteString(section)
		sb.WriteString(promptText[idx:])
		return strings.Clone(sb.String())
	}

	if strings.TrimSpace(promptText) == "" {
		return section
	}

	sb.Grow(len(promptText) + len(section) + 2)
	sb.WriteString(promptText)
	sb.WriteString("\n\n")
	sb.WriteString(strings.TrimRight(section, "\n"))
	return strings.Clone(sb.String())
}

func findUserMarker(promptText string) (string, int) {
	marker := "<user_request>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return marker, idx
	}
	marker = "<user_message>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return marker, idx
	}
	return "", -1
}
