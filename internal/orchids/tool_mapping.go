// Package client provides tool name mapping between Claude Code and Orchids.
package orchids

import (
	"strings"
	"sync"
)

// ToolMapper handles bidirectional tool name mapping.
type ToolMapper struct {
	// Claude Code → Orchids 标准名
	toOrchids map[string]string
	// Orchids → Claude Code 标准名
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
	tm.addMapping("ListDir", "LS")
	tm.addMapping("list_dir", "LS")
	tm.addMapping("list_directory", "LS")
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
	tm.addMapping("search", "WebSearch")

	// MCP 工具
	tm.addMapping("query-docs", "mcp__context7__query-docs")
	tm.addMapping("resolve-library-id", "mcp__context7__resolve-library-id")
	tm.addMapping("mcp__context7__query-docs", "mcp__context7__query-docs")
	tm.addMapping("mcp__context7__resolve-library-id", "mcp__context7__resolve-library-id")

	// Orchids 事件 → Claude Code 工具名映射
	tm.fromOrchids["edit_file"] = "Edit"
	tm.fromOrchids["todo_write"] = "TodoWrite"
	tm.fromOrchids["Edit"] = "Edit"
	tm.fromOrchids["Read"] = "Read"
	tm.fromOrchids["Write"] = "Write"
	tm.fromOrchids["Bash"] = "Bash"
	tm.fromOrchids["LS"] = "LS"
	tm.fromOrchids["Glob"] = "Glob"
	tm.fromOrchids["Grep"] = "Grep"
	tm.fromOrchids["TodoWrite"] = "TodoWrite"

	return tm
}

func (tm *ToolMapper) addMapping(from, to string) {
	tm.toOrchids[from] = to
	tm.toOrchids[strings.ToLower(from)] = to
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

// FromOrchids maps an Orchids tool name to Claude Code standard name.
func (tm *ToolMapper) FromOrchids(name string) string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if mapped, ok := tm.fromOrchids[name]; ok {
		return mapped
	}
	return name
}

// IsBlocked checks if a tool should be blocked.
// 可以在这里添加工具过滤逻辑
var blockedTools = map[string]bool{
	"web_search":               true,
	"WebSearch":                true,
	"SQL":                      true,
	"server":                   true,
	"suggest_plan":             true,
	"suggest_create_plan":      true,
	"suggest_new_conversation": true,
	"read_mcp_resource":        true,
	// 添加其他需要屏蔽的云端工具
}

// IsBlocked returns true if the tool should be blocked.
func (tm *ToolMapper) IsBlocked(name string) bool {
	lower := strings.ToLower(name)
	return blockedTools[name] || blockedTools[lower]
}

// NormalizeToolName standardizes tool name for consistent handling.
func NormalizeToolName(name string) string {
	return DefaultToolMapper.ToOrchids(name)
}
