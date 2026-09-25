package toolname

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

// ToolMapper manages the mapping between client tool definitions and canonical tool names.
type ToolMapper struct {
	Tools []map[string]interface{}
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

// NormalizeToolNameFallback provides backward compatibility for the handler package.
func NormalizeToolNameFallback(name string) string {
	if mapped, ok := normalizedToolNameFallbacks[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return name
}

func MapToolNameToClient(upstreamName string, clientTools []interface{}, toolMapper *ToolMapper) string {
	normalized := NormalizeToolName(upstreamName)
	if normalized.Original == "" {
		return upstreamName
	}

	tools := toolMapperClientTools(clientTools, toolMapper)

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name == "" {
			continue
		}
		if strings.ToLower(name) == normalized.Lowercase || toSnakeCase(strings.ToLower(name)) == normalized.SnakeCase {
			return name
		}
		fallbackName := strings.ToLower(strings.TrimSpace(NormalizeToolNameFallback(name)))
		if fallbackName == normalized.Lowercase || toSnakeCase(fallbackName) == normalized.SnakeCase {
			return name
		}
	}

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name == "" {
			continue
		}
		for _, alias := range getToolAliases(tool) {
			if alias == normalized.SnakeCase || alias == normalized.Lowercase || strings.EqualFold(alias, upstreamName) {
				return name
			}
		}
	}

	if aliases, ok := toolAliases[normalized.SnakeCase]; ok {
		for _, tool := range tools {
			name := toolSpecName(tool)
			if name == "" {
				continue
			}
			for _, alias := range aliases {
				toolSnake := toSnakeCase(strings.ToLower(name))
				if toolSnake == alias || strings.ToLower(name) == alias || strings.EqualFold(name, alias) {
					return name
				}
			}
		}
	}

	for _, tool := range tools {
		name := toolSpecName(tool)
		if name == "" {
			continue
		}
		toolSnake := toSnakeCase(strings.ToLower(name))
		if aliases, ok := toolAliases[toolSnake]; ok {
			for _, alias := range aliases {
				if alias == normalized.SnakeCase || alias == normalized.Lowercase {
					return name
				}
			}
		}
	}

	return MapToolToAnthropic(upstreamName)
}

func MapToolToAnthropic(upstreamName string) string {
	switch strings.TrimSpace(upstreamName) {
	case "str_replace_editor", "bash", "computer", "text_editor":
		return strings.TrimSpace(upstreamName)
	}
	return upstreamName
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

func toolSpecName(tool map[string]interface{}) string {
	return strings.TrimSpace(extractToolName(tool))
}

func extractToolName(tool map[string]interface{}) string {
	name, _, _ := ExtractToolSpecFields(tool)
	return name
}

var toolAliases = map[string][]string{
	"id":      {"text"},
	"name":    {"text"},
	"content": {"code"},
	"source":  {"input"},
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

// ExtractToolSpecFields reads the common OpenAI/Anthropic tool definition shapes.
func ExtractToolSpecFields(tool interface{}) (string, string, map[string]interface{}) {
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
