package handler

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"orchids-api/internal/orchids"
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
			strVal = strings.TrimSpace(strVal)

			if strVal == "true" {
				input[key] = true
				fixed = true
				continue
			} else if strVal == "false" {
				input[key] = false
				fixed = true
				continue
			}

			if num, err := strconv.ParseInt(strVal, 10, 64); err == nil {
				input[key] = num
				fixed = true
				continue
			}

			if fnum, err := strconv.ParseFloat(strVal, 64); err == nil {
				input[key] = fnum
				fixed = true
				continue
			}

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

type toolNameInfo struct {
	name    string
	lowered string
	short   string
	props   map[string]struct{}
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
		name, _ := tm["name"].(string)
		name = strings.TrimSpace(name)
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
		if schema, ok := tm["input_schema"].(map[string]interface{}); ok {
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

func shouldPreflightTools(text string) bool {
	if text == "" {
		return false
	}
	// Always run preflight to ensure model has context about project structure
	// This prevents hallucinations about the tech stack (e.g. thinking it's Next.js/FastAPI when it's Go)
	return true
}

func filterSupportedTools(tools []interface{}) []interface{} {
	if len(tools) == 0 {
		return tools
	}
	supported := map[string]bool{
		"read":            true,
		"write":           true,
		"edit":            true,
		"bash":            true,
		"runcommand":      true,
		"run_command":     true,
		"execute_command": true,
		"execute-command": true,
		"launch-process":  true,
		"glob":            true,
		"grep":            true,
		"ls":              true,
		"list":            true,
		"todowrite":       true,
	}
	var filtered []interface{}
	for _, tool := range tools {
		tm, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		if supported[strings.ToLower(strings.TrimSpace(name))] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func formatLocalToolResults(results []safeToolResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("以下 tool_result 来自用户本地环境，具有权威性；回答必须以此为准，不要使用你自己的环境推断。")
	for _, result := range results {
		inputJSON, _ := json.Marshal(result.input)
		errorAttr := ""
		if result.isError {
			errorAttr = ` is_error="true"`
		}
		b.WriteString("\n<tool_use id=\"")
		b.WriteString(result.call.id)
		b.WriteString("\" name=\"")
		b.WriteString(result.call.name)
		b.WriteString("\">\n")
		b.WriteString(string(inputJSON))
		b.WriteString("\n</tool_use>\n<tool_result tool_use_id=\"")
		b.WriteString(result.call.id)
		b.WriteString("\"")
		b.WriteString(errorAttr)
		b.WriteString(">\n")
		b.WriteString(result.output)
		b.WriteString("\n</tool_result>")
	}
	return b.String()
}

func injectLocalContext(promptText string, context string) string {
	context = strings.TrimSpace(context)
	if context == "" {
		return promptText
	}
	section := "<local_context>\n" + context + "\n</local_context>\n\n"
	marker := "<user_request>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return promptText[:idx] + section + promptText[idx:]
	}
	if strings.TrimSpace(promptText) == "" {
		return section
	}
	return promptText + "\n\n" + strings.TrimRight(section, "\n")
}

func injectToolGate(promptText string, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return promptText
	}
	section := "<tool_gate>\n" + message + "\n</tool_gate>\n\n"
	marker := "<user_request>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return promptText[:idx] + section + promptText[idx:]
	}
	if strings.TrimSpace(promptText) == "" {
		return section
	}
	return promptText + "\n\n" + strings.TrimRight(section, "\n")
}

func extractPreflightPwd(results []safeToolResult) string {
	if len(results) == 0 {
		return ""
	}
	if results[0].isError {
		return ""
	}
	return strings.TrimSpace(results[0].output)
}

func buildLocalFallbackResponse(results []safeToolResult) string {
	pwd := extractPreflightPwd(results)
	findOutput := ""
	if len(results) > 1 && !results[1].isError {
		findOutput = results[1].output
	}
	topEntries := extractTopLevelEntries(findOutput, 12)
	projectName := ""
	if pwd != "" {
		projectName = filepath.Base(pwd)
	}

	var b strings.Builder
	if projectName != "" {
		b.WriteString("这是 `")
		b.WriteString(projectName)
		b.WriteString("` 项目。")
	} else {
		b.WriteString("这是当前目录下的项目。")
	}
	if pwd != "" {
		b.WriteString("\n当前项目目录: ")
		b.WriteString(pwd)
	}
	if len(topEntries) > 0 {
		b.WriteString("\n顶层目录/文件: ")
		b.WriteString(strings.Join(topEntries, ", "))
	}
	if containsEntry(topEntries, "go.mod") {
		b.WriteString("\n从目录结构看，这是一个 Go 项目（包含 go.mod/go.sum、cmd、internal、web 等）。")
	} else if len(topEntries) > 0 {
		b.WriteString("\n从目录结构看，这是一个后端服务项目，建议查看 README.md 获取完整说明。")
	}
	b.WriteString("\n如需更准确的项目介绍，请告知你想关注的模块或具体文件。")
	return b.String()
}

func extractTopLevelEntries(findOutput string, limit int) []string {
	if findOutput == "" {
		return nil
	}
	seen := map[string]bool{}
	var entries []string
	for _, line := range strings.Split(findOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "." {
			continue
		}
		line = strings.TrimPrefix(line, "./")
		if line == "" || strings.Contains(line, "/") {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		entries = append(entries, line)
		if limit > 0 && len(entries) >= limit {
			break
		}
	}
	return entries
}

func containsEntry(entries []string, target string) bool {
	for _, entry := range entries {
		if entry == target {
			return true
		}
	}
	return false
}
