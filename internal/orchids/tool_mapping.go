package orchids

import "strings"

var normalizedToolNameFallbacks = map[string]string{
	"str_replace_editor": "Edit",
	"edit":               "Edit",
	"apply_file_diffs":   "Edit",

	"view":       "Read",
	"readfile":   "Read",
	"read_file":  "Read",
	"read_files": "Read",
	"read":       "Read",

	"listdir":        "Glob",
	"list_dir":       "Glob",
	"list_directory": "Glob",
	"ls":             "Glob",
	"globtool":       "Glob",
	"glob":           "Glob",
	"find_files":     "Glob",
	"file_glob":      "Glob",
	"file_glob_v2":   "Glob",

	"ripgreptool":     "Grep",
	"ripgrep":         "Grep",
	"search_code":     "Grep",
	"search_codebase": "Grep",
	"grep":            "Grep",

	"exec":              "Bash",
	"execute":           "Bash",
	"execute_command":   "Bash",
	"execute-command":   "Bash",
	"run_command":       "Bash",
	"runcommand":        "Bash",
	"launch-process":    "Bash",
	"run_shell_command": "Bash",
	"shell":             "Bash",
	"bash":              "Bash",

	"writefile":   "Write",
	"write_file":  "Write",
	"create_file": "Write",
	"createfile":  "Write",
	"save-file":   "Write",
	"write":       "Write",

	"update_todo_list": "TodoWrite",
	"todo":             "TodoWrite",
	"todo_write":       "TodoWrite",
	"todowrite":        "TodoWrite",

	"web_fetch":                "web_fetch",
	"webfetch":                 "web_fetch",
	"fetch":                    "web_fetch",
	"builtin_web_fetch":        "web_fetch",
	"mcp__fetch__fetch":        "web_fetch",
	"mcp__tavily__web_extract": "web_fetch",

	"web_search":              "web_search",
	"websearch":               "web_search",
	"builtin_web_search":      "web_search",
	"mcp__tavily__web_search": "web_search",
	"mcp__brave__web_search":  "web_search",

	"ask_followup_question": "AskUserQuestion",
	"ask":                   "AskUserQuestion",

	"enter_plan_mode": "EnterPlanMode",
	"exit_plan_mode":  "ExitPlanMode",

	"new_task":       "Task",
	"agent":          "Task",
	"subagent":       "Task",
	"subagents":      "Task",
	"spawn_agent":    "Task",
	"spawn_subagent": "Task",
	"session_spawn":  "Task",
	"sessions_spawn": "Task",

	"task_output": "TaskOutput",
	"task_stop":   "TaskStop",

	"use_skill": "Skill",
	"skill":     "Skill",
}

func buildClientToolMapper(clientTools []interface{}) *ToolMapper {
	tools := toolMapsFromInterfaces(clientTools)
	if len(tools) == 0 {
		return nil
	}
	tm := &ToolMapper{Tools: tools}
	tm.buildIndex()
	return tm
}

func (tm *ToolMapper) buildIndex() {
	if tm == nil {
		return
	}
	tm.index = make(map[string]map[string]interface{}, len(tm.Tools))
	for _, tool := range tm.Tools {
		name := toolSpecName(tool)
		if name == "" {
			continue
		}
		tm.index[strings.ToLower(name)] = tool
	}
}

// ToolMapper manages the mapping between client tool definitions and Orchids tool names.
type ToolMapper struct {
	Tools []map[string]interface{}
	index map[string]map[string]interface{}
}

// NormalizedTool holds the normalized form of a tool name for matching.
type NormalizedTool struct {
	Original  string
	Lowercase string
	SnakeCase string
}

func NormalizeToolName(name string) *NormalizedTool {
	if name == "" {
		return &NormalizedTool{}
	}
	lower := strings.ToLower(name)
	snake := toSnakeCase(lower)
	return &NormalizedTool{
		Original:  name,
		Lowercase: lower,
		SnakeCase: snake,
	}
}

// NormalizeToolNameFallback provides backward compatibility for the warp and handler packages.
func NormalizeToolNameFallback(name string) string {
	if mapped, ok := normalizedToolNameFallbacks[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return name
}

func MapToolNameToClient(orchidsName string, clientTools []interface{}, toolMapper *ToolMapper) string {
	normalized := NormalizeToolName(orchidsName)
	if normalized.Original == "" {
		return orchidsName
	}

	tools := toolMapperClientTools(clientTools, toolMapper)

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name != "" {
			if strings.ToLower(name) == normalized.Lowercase {
				return name
			}
			if toSnakeCase(strings.ToLower(name)) == normalized.SnakeCase {
				return name
			}
			fallbackName := NormalizeToolNameFallback(name)
			if strings.ToLower(strings.TrimSpace(fallbackName)) == normalized.Lowercase {
				return name
			}
			if toSnakeCase(strings.ToLower(strings.TrimSpace(fallbackName))) == normalized.SnakeCase {
				return name
			}
		}
	}

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name != "" {
			if toolAliases := getToolAliases(tool); toolAliases != nil {
				for _, alias := range toolAliases {
					if alias == normalized.SnakeCase || alias == normalized.Lowercase || strings.EqualFold(alias, orchidsName) {
						return name
					}
				}
			}
		}
	}

	if aliases, ok := orchidsToolAliases[normalized.SnakeCase]; ok {
		for _, tool := range tools {
			name := toolSpecName(tool)
			if name != "" {
				for _, alias := range aliases {
					toolSnake := toSnakeCase(strings.ToLower(name))
					if toolSnake == alias || strings.ToLower(name) == alias || strings.EqualFold(name, alias) {
						return name
					}
				}
			}
		}
	}

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name != "" {
			toolSnake := toSnakeCase(strings.ToLower(name))
			if toolAliases, ok := orchidsToolAliases[toolSnake]; ok {
				for _, alias := range toolAliases {
					if alias == normalized.SnakeCase || alias == normalized.Lowercase {
						return name
					}
				}
			}
		}
	}

	return MapOrchidsToolToAnthropic(orchidsName)
}

func TransformToolInput(toolName, clientName string, input map[string]interface{}) map[string]interface{} {
	if input == nil {
		input = make(map[string]interface{})
	}

	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	lowerClient := strings.ToLower(strings.TrimSpace(clientName))
	if lowerTool == "" {
		return copyToolInput(input)
	}

	if lowerTool == "ls" || lowerTool == "list" || lowerTool == "glob" {
		if strings.Contains(lowerClient, "/") {
			return copyToolInput(input)
		}
	}

	if lowerTool == "ls" && lowerClient == "glob" {
		result := copyToolInput(input)
		if _, ok := result["content"]; !ok {
			result["content"] = []interface{}{}
		}
		return result
	}

	if lowerTool == "ls" && strings.Contains(lowerClient, ".") {
		return copyToolInput(input)
	}

	if lowerTool == "read" || lowerTool == "readfile" {
		result := make(map[string]interface{})
		filePath := mapStringValue(input, "file_path", "path")
		if filePath == "" {
			filePath = mapStringValue(input, "content")
		}
		result["file_path"] = filePath
		for key, value := range input {
			if key == "file_path" || key == "path" {
				continue
			}
			result[key] = value
		}
		return result
	}

	if lowerTool == "bash" || strings.Contains(lowerTool, "edit") || strings.Contains(lowerTool, "write") {
		result := make(map[string]interface{})
		if content := mapStringValue(input, "content"); content != "" {
			result["content"] = content
		}
		if path := mapStringValue(input, "path"); path != "" {
			if existing, ok := result["content"].(string); ok && existing != "" {
				result["content"] = existing + "\n" + path
			} else {
				result["content"] = path
			}
		} else if _, ok := result["content"]; !ok {
			result["content"] = ""
		}
		return result
	}

	return copyToolInput(input)
}

func MapOrchidsToolToAnthropic(orchidsName string) string {
	if mapped, ok := orchidsToAnthropicMap[strings.TrimSpace(orchidsName)]; ok {
		return mapped
	}
	return orchidsName
}

func toolMapsFromInterfaces(clientTools []interface{}) []map[string]interface{} {
	if len(clientTools) == 0 {
		return nil
	}

	out := make([]map[string]interface{}, 0, len(clientTools))
	for _, raw := range clientTools {
		tool, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, tool)
	}
	return out
}

func toolMapperClientTools(clientTools []interface{}, toolMapper *ToolMapper) []map[string]interface{} {
	if toolMapper != nil && len(toolMapper.Tools) > 0 {
		return toolMapper.Tools
	}
	return toolMapsFromInterfaces(clientTools)
}

func copyToolInput(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	cloned := make(map[string]interface{}, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func toolSpecName(tool map[string]interface{}) string {
	return strings.TrimSpace(extractToolName(tool))
}

func extractToolName(tool map[string]interface{}) string {
	name, _, _ := extractToolSpecFields(tool)
	return name
}

var orchidsToolAliases = map[string][]string{
	"id":      {"text"},
	"name":    {"text"},
	"content": {"code"},
	"source":  {"input"},
}

var orchidsToAnthropicMap = map[string]string{
	"str_replace_editor": "str_replace_editor",
	"bash":               "bash",
	"computer":           "computer",
	"text_editor":        "text_editor",
}

func toSnakeCase(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var out strings.Builder
	for i, r := range value {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('_')
			}
			out.WriteByte(byte(r - 'A' + 'a'))
			continue
		}
		if r == '-' || r == ' ' {
			out.WriteByte('_')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func getToolAliases(tool map[string]interface{}) []string {
	if len(tool) == 0 {
		return nil
	}
	if aliases := extractAliasStrings(tool["aliases"]); len(aliases) > 0 {
		return aliases
	}
	if fn, ok := tool["function"].(map[string]interface{}); ok {
		if aliases := extractAliasStrings(fn["aliases"]); len(aliases) > 0 {
			return aliases
		}
	}
	return nil
}

func extractAliasStrings(raw interface{}) []string {
	aliases, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		value, ok := alias.(string)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}
