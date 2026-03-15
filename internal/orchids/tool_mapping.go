// Package client provides tool name mapping between Claude Code and Orchids.
package orchids

import (
	"github.com/goccy/go-json"
	"strings"
	"sync"
)

// ToolMapper handles bidirectional tool name mapping.
type ToolMapper struct {
	// Claude Code → Orchids 标准名
	toOrchids   map[string]string
	fromOrchids map[string]string
	mu          sync.RWMutex
}

// DefaultToolMapper is the global tool mapper instance.
var DefaultToolMapper = NewToolMapper()

// NewToolMapper creates a new ToolMapper with default mappings.
func NewToolMapper() *ToolMapper {
	tm := &ToolMapper{
		toOrchids:   make(map[string]string),
		fromOrchids: make(map[string]string),
	}

	// Claude Code → Orchids 映射
	tm.addMapping("Str_Replace_Editor", "Edit")
	tm.addMapping("str_replace_editor", "Edit")
	tm.addMapping("View", "Read")
	tm.addMapping("view", "Read")
	tm.addMapping("ReadFile", "Read")
	tm.addMapping("read_file", "Read")
	tm.addMapping("ListDir", "Glob")
	tm.addMapping("list_dir", "Glob")
	tm.addMapping("list_directory", "Glob")
	tm.addMapping("LS", "Glob")
	tm.addMapping("RipGrepTool", "Grep")
	tm.addMapping("ripgrep", "Grep")
	tm.addMapping("search_code", "Grep")
	tm.addMapping("SearchCode", "Grep")
	tm.addMapping("GlobTool", "Glob")
	tm.addMapping("glob", "Glob")
	tm.addMapping("find_files", "Glob")
	tm.addMapping("Execute", "Bash")
	tm.addMapping("execute", "Bash")
	tm.addMapping("execute_command", "Bash")
	tm.addMapping("execute-command", "Bash")
	tm.addMapping("run_command", "Bash")
	tm.addMapping("runcommand", "Bash")
	tm.addMapping("RunCommand", "Bash")
	tm.addMapping("launch-process", "Bash")
	tm.addMapping("WriteFile", "Write")
	tm.addMapping("write_file", "Write")
	tm.addMapping("CreateFile", "Write")
	tm.addMapping("create_file", "Write")
	tm.addMapping("save-file", "Write")
	tm.addMapping("Write", "Write")

	// Warp 内置工具名 → 标准工具名
	tm.addMapping("run_shell_command", "Bash")
	tm.addMapping("write_to_long_running_shell_command", "Bash")
	tm.addMapping("search_codebase", "Grep")
	tm.addMapping("grep", "Grep")
	tm.addMapping("file_glob", "Glob")
	tm.addMapping("file_glob_v2", "Glob")
	tm.addMapping("read_files", "Read")
	tm.addMapping("apply_file_diffs", "Edit")

	// 计划与任务工具
	tm.addMapping("update_todo_list", "TodoWrite")
	tm.addMapping("todo", "TodoWrite")
	tm.addMapping("todo_write", "TodoWrite")
	tm.addMapping("todowrite", "TodoWrite")
	tm.addMapping("ask_followup_question", "AskUserQuestion")
	tm.addMapping("ask", "AskUserQuestion")
	tm.addMapping("enter_plan_mode", "EnterPlanMode")
	tm.addMapping("exit_plan_mode", "ExitPlanMode")
	tm.addMapping("new_task", "Task")
	tm.addMapping("task_output", "TaskOutput")
	tm.addMapping("task_stop", "TaskStop")
	tm.addMapping("use_skill", "Skill")
	tm.addMapping("skill", "Skill")

	// Web 工具
	tm.addMapping("web_fetch", "WebFetch")
	tm.addMapping("webfetch", "WebFetch")
	tm.addMapping("fetch", "WebFetch")

	// MCP 工具
	tm.addMapping("query-docs", "mcp__context7__query-docs")
	tm.addMapping("resolve-library-id", "mcp__context7__resolve-library-id")
	tm.addMapping("mcp__context7__query-docs", "mcp__context7__query-docs")
	tm.addMapping("mcp__context7__resolve-library-id", "mcp__context7__resolve-library-id")

	tm.addCanonical("Bash")
	tm.addCanonical("Read")
	tm.addCanonical("Edit")
	tm.addCanonical("Write")
	tm.addCanonical("Glob")
	tm.addCanonical("Grep")
	tm.addCanonical("TodoWrite")
	tm.addCanonical("AskUserQuestion")
	tm.addCanonical("EnterPlanMode")
	tm.addCanonical("ExitPlanMode")
	tm.addCanonical("Task")
	tm.addCanonical("TaskOutput")
	tm.addCanonical("TaskStop")
	tm.addCanonical("Skill")
	tm.addCanonical("WebFetch")
	tm.addCanonical("mcp__context7__query-docs")
	tm.addCanonical("mcp__context7__resolve-library-id")

	return tm
}

func (tm *ToolMapper) addMapping(from, to string) {
	tm.toOrchids[from] = to
	tm.toOrchids[strings.ToLower(from)] = to
}

func (tm *ToolMapper) addCanonical(name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	tm.fromOrchids[strings.ToLower(name)] = name
}

// ToOrchids maps a Claude Code tool name to Orchids standard name.
func (tm *ToolMapper) ToOrchids(name string) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if mapped, ok := tm.toOrchids[name]; ok {
		return mapped
	}
	if mapped, ok := tm.toOrchids[strings.ToLower(name)]; ok {
		return mapped
	}
	return name // 未知工具保持原名
}

func (tm *ToolMapper) FromOrchids(name string) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if mapped, ok := tm.fromOrchids[strings.ToLower(strings.TrimSpace(name))]; ok {
		return mapped
	}
	return name
}

// NormalizeToolName standardizes tool name for consistent handling.
func NormalizeToolName(name string) string {
	return DefaultToolMapper.ToOrchids(name)
}

func (tm *ToolMapper) IsBlocked(string) bool {
	return false
}

func MapToolNameToClient(orchidsName string, clientTools []interface{}) string {
	rawName := strings.TrimSpace(orchidsName)
	if rawName == "" {
		return rawName
	}

	standardName := NormalizeToolName(rawName)
	rawLower := strings.ToLower(rawName)
	rawSnake := toSnakeCase(rawLower)
	tools := toolMapsFromInterfaces(clientTools)
	if len(tools) == 0 {
		return MapOrchidsToolToAnthropic(rawName)
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if name == rawName {
			return name
		}
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, rawName) {
			return name
		}
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.ToLower(name) == rawLower {
			return name
		}
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if toSnakeCase(strings.ToLower(name)) == rawSnake {
			return name
		}
	}

	if aliases, ok := orchidsToolAliases[rawSnake]; ok {
		for _, tool := range tools {
			name, _, _ := extractToolSpecFields(tool)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			toolSnake := toSnakeCase(strings.ToLower(name))
			for _, alias := range aliases {
				if toolSnake == alias || strings.ToLower(name) == alias {
					return name
				}
			}
		}
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		toolSnake := toSnakeCase(strings.ToLower(name))
		if aliases, ok := orchidsToolAliases[toolSnake]; ok {
			for _, alias := range aliases {
				if alias == rawSnake || alias == rawLower {
					return name
				}
			}
		}
	}

	if aliases, ok := orchidsToolAliases[rawSnake]; ok {
		for _, tool := range tools {
			name, _, _ := extractToolSpecFields(tool)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			for _, alias := range aliases {
				if strings.EqualFold(name, alias) {
					return name
				}
			}
		}
	}

	for _, tool := range tools {
		name, _, _ := extractToolSpecFields(tool)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.EqualFold(NormalizeToolName(name), standardName) {
			return name
		}
	}

	return MapOrchidsToolToAnthropic(rawName)
}

func TransformToolInput(toolName, clientName string, input map[string]interface{}) map[string]interface{} {
	if input == nil {
		input = make(map[string]interface{})
	}

	lowerTool := strings.ToLower(strings.TrimSpace(toolName))
	lowerClient := strings.ToLower(strings.TrimSpace(clientName))
	if lowerTool == "" {
		lowerTool = strings.ToLower(strings.TrimSpace(NormalizeToolName(clientName)))
	}
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
	normalized := NormalizeToolName(orchidsName)
	if mapped, ok := orchidsToAnthropicMap[strings.TrimSpace(normalized)]; ok {
		return mapped
	}
	return DefaultToolMapper.FromOrchids(normalized)
}

func transformToolInputJSON(toolName, clientName, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return raw
	}

	normalized := TransformToolInput(toolName, clientName, input)
	if normalized == nil {
		return raw
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func toolMapsFromInterfaces(clientTools []interface{}) []map[string]interface{} {
	if len(clientTools) == 0 {
		return nil
	}

	out := make([]map[string]interface{}, 0, len(clientTools))
	for _, raw := range clientTools {
		if tool, ok := raw.(map[string]interface{}); ok {
			out = append(out, tool)
		}
	}
	return out
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
