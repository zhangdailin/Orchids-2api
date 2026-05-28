package handler

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/tiktoken"
)

func legacyEstimateCompactedToolsTokens(tools []interface{}) int {
	compacted := compactIncomingTools(tools)
	if len(compacted) == 0 {
		return 0
	}
	raw, err := json.Marshal(compacted)
	if err != nil {
		return 0
	}
	return tiktoken.EstimateTextTokens(string(raw))
}

func sampleIncomingTools() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Write",
				"description": "write file content safely",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"file_path": map[string]interface{}{"type": "string"},
						"content":   map[string]interface{}{"type": "string", "description": "utf-8 内容"},
					},
				},
			},
		},
		map[string]interface{}{
			"name":        "Read",
			"description": "read file content",
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
			},
		},
	}
}

func TestEstimateCompactedToolsTokensMatchesLegacy(t *testing.T) {
	tools := sampleIncomingTools()
	if got, want := estimateCompactedToolsTokens(tools), legacyEstimateCompactedToolsTokens(tools); got != want {
		t.Fatalf("estimateCompactedToolsTokens=%d want=%d", got, want)
	}
}

func BenchmarkEstimateCompactedToolsTokens_Legacy(b *testing.B) {
	tools := sampleIncomingTools()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = legacyEstimateCompactedToolsTokens(tools)
	}
}

func BenchmarkEstimateCompactedToolsTokens_Current(b *testing.B) {
	tools := sampleIncomingTools()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = estimateCompactedToolsTokens(tools)
	}
}

func TestCompactIncomingTools_FiltersUnsupportedAndMinimizesSupportedSchemas(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Agent",
				"description": strings.Repeat("unsupported", 40),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"prompt": map[string]interface{}{"type": "string", "description": "very long prompt description"},
					},
				},
			},
		},
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Bash",
				"description": strings.Repeat("shell command ", 30),
				"parameters": map[string]interface{}{
					"type":     "object",
					"required": []interface{}{"command", "ignored"},
					"properties": map[string]interface{}{
						"command":                   map[string]interface{}{"type": "string", "description": "command to run"},
						"description":               map[string]interface{}{"type": "string", "description": "user-facing summary"},
						"dangerouslyDisableSandbox": map[string]interface{}{"type": "boolean", "description": "disable sandbox"},
						"timeout":                   map[string]interface{}{"type": "number", "description": "milliseconds"},
						"ignored":                   map[string]interface{}{"type": "string", "description": "should be removed"},
					},
				},
			},
		},
		map[string]interface{}{
			"name":        "View",
			"description": "Read file contents with offsets",
			"input_schema": map[string]interface{}{
				"type":     "object",
				"required": []interface{}{"file_path", "ignored"},
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string", "description": "path to read"},
					"offset":    map[string]interface{}{"type": "number", "description": "line offset"},
					"limit":     map[string]interface{}{"type": "number"},
					"ignored":   map[string]interface{}{"type": "string", "description": "remove me"},
				},
			},
		},
	}

	got := compactIncomingTools(tools)
	if len(got) != 2 {
		t.Fatalf("compactIncomingTools() len=%d want=2", len(got))
	}

	bashTool, ok := got[0].(map[string]interface{})
	if !ok {
		t.Fatalf("bash tool type = %T", got[0])
	}
	bashFn, ok := bashTool["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("bash function type = %T", bashTool["function"])
	}
	if gotName, _ := bashFn["name"].(string); gotName != "Bash" {
		t.Fatalf("bash function name = %q want %q", gotName, "Bash")
	}
	if desc, _ := bashFn["description"].(string); !strings.HasSuffix(desc, "...[truncated]") {
		t.Fatalf("bash description = %q, want truncated suffix", desc)
	}
	bashParams, ok := bashFn["parameters"].(map[string]interface{})
	if !ok {
		t.Fatalf("bash parameters type = %T", bashFn["parameters"])
	}
	bashProps, ok := bashParams["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("bash properties type = %T", bashParams["properties"])
	}
	for _, key := range []string{"command", "description", "dangerouslyDisableSandbox", "timeout"} {
		if _, exists := bashProps[key]; !exists {
			t.Fatalf("bash properties missing %q", key)
		}
	}
	if _, exists := bashProps["ignored"]; exists {
		t.Fatalf("bash properties unexpectedly kept ignored field")
	}
	if cmdSchema, ok := bashProps["command"].(map[string]interface{}); ok {
		if _, exists := cmdSchema["description"]; exists {
			t.Fatalf("bash command schema unexpectedly kept description")
		}
	}
	if required, ok := bashParams["required"].([]interface{}); ok {
		if len(required) != 1 || required[0] != "command" {
			t.Fatalf("bash required = %#v want [command]", required)
		}
	}

	viewTool, ok := got[1].(map[string]interface{})
	if !ok {
		t.Fatalf("view tool type = %T", got[1])
	}
	if gotName, _ := viewTool["name"].(string); gotName != "View" {
		t.Fatalf("view tool name = %q want %q", gotName, "View")
	}
	viewSchema, ok := viewTool["input_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("view schema type = %T", viewTool["input_schema"])
	}
	viewProps, ok := viewSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("view properties type = %T", viewSchema["properties"])
	}
	for _, key := range []string{"file_path", "offset", "limit"} {
		if _, exists := viewProps[key]; !exists {
			t.Fatalf("view properties missing %q", key)
		}
	}
	if _, exists := viewProps["ignored"]; exists {
		t.Fatalf("view properties unexpectedly kept ignored field")
	}
	if required, ok := viewSchema["required"].([]interface{}); ok {
		if len(required) != 1 || required[0] != "file_path" {
			t.Fatalf("view required = %#v want [file_path]", required)
		}
	}
}

func TestEstimateCompactedToolsTokens_IgnoresUnsupportedTools(t *testing.T) {
	supported := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "Bash",
				"description": "run shell command",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}
	mixed := append([]interface{}{}, supported...)
	mixed = append(mixed, map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "Agent",
			"description": strings.Repeat("very expensive unsupported tool ", 100),
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{"type": "string", "description": strings.Repeat("payload ", 100)},
				},
			},
		},
	})

	if got, want := estimateCompactedToolsTokens(mixed), estimateCompactedToolsTokens(supported); got != want {
		t.Fatalf("estimateCompactedToolsTokens(mixed)=%d want %d", got, want)
	}
}

func TestSupportedToolNames_NormalizesAndOrdersTools(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "todo_write"},
		map[string]interface{}{"name": "run_command"},
		map[string]interface{}{"name": "View"},
		map[string]interface{}{"name": "Agent"},
		map[string]interface{}{"name": "Skill"},
		map[string]interface{}{"name": "Read"},
	}

	got := supportedToolNames(tools)
	want := []string{"Read", "Bash", "Task", "Skill"}
	if len(got) != len(want) {
		t.Fatalf("supportedToolNames len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedToolNames[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestDeclaredToolNames_KeepCustomAndCanonicalAliases(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "workspace_search"},
		map[string]interface{}{"name": "read_files"},
		map[string]interface{}{"name": "Read"},
	}

	got := declaredToolNames(tools)
	want := []string{"workspace_search", "read_files", "Read"}
	if len(got) != len(want) {
		t.Fatalf("declaredToolNames len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("declaredToolNames[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestPassthroughAllowedToolNames_DropsUnsupportedMetaTools(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "Read"},
		map[string]interface{}{"name": "run_command"},
		map[string]interface{}{"name": "Agent"},
		map[string]interface{}{"name": "Skill"},
		map[string]interface{}{"name": "new_task"},
		map[string]interface{}{"name": "task_output"},
	}

	got := passthroughAllowedToolNames(tools, true)
	want := []string{"Read", "Bash", "Task", "Skill"}
	if len(got) != len(want) {
		t.Fatalf("passthroughAllowedToolNames len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("passthroughAllowedToolNames[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestPassthroughAllowedToolNames_ReturnsNilWhenRequestOmitsTools(t *testing.T) {
	got := passthroughAllowedToolNames(nil, true)
	if got != nil {
		t.Fatalf("passthroughAllowedToolNames(nil, true) = %#v want nil", got)
	}
}

func TestValidationAllowedToolNames_UsesOriginalDeclaredToolsWhenPresent(t *testing.T) {
	effective := []interface{}{
		map[string]interface{}{"name": "Read"},
		map[string]interface{}{"name": "Task"},
	}
	original := []interface{}{
		map[string]interface{}{"name": "read"},
		map[string]interface{}{"name": "web_search"},
		map[string]interface{}{"name": "sessions_spawn"},
	}

	got := validationAllowedToolNames(effective, original, true)
	want := []string{"read", "web_search", "sessions_spawn", "Task"}
	if len(got) != len(want) {
		t.Fatalf("validationAllowedToolNames len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("validationAllowedToolNames[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestValidationAllowedToolNames_TreatsExecAsBash(t *testing.T) {
	effective := []interface{}{
		map[string]interface{}{"name": "Read"},
		map[string]interface{}{"name": "Bash"},
	}
	original := []interface{}{
		map[string]interface{}{"name": "read"},
		map[string]interface{}{"name": "exec"},
	}

	got := validationAllowedToolNames(effective, original, true)
	want := []string{"read", "exec", "Bash"}
	if len(got) != len(want) {
		t.Fatalf("validationAllowedToolNames len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("validationAllowedToolNames[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestSupportedToolNames_MapsOpenClawSubagentsToTask(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "read"},
		map[string]interface{}{"name": "subagents"},
		map[string]interface{}{"name": "sessions_spawn"},
	}

	got := supportedToolNames(tools)
	want := []string{"Read", "Task"}
	if len(got) != len(want) {
		t.Fatalf("supportedToolNames(subagents) len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedToolNames(subagents)[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestSupportedToolNames_MapsOpenClawExecToBash(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "read"},
		map[string]interface{}{"name": "exec"},
	}

	got := supportedToolNames(tools)
	want := []string{"Read", "Bash"}
	if len(got) != len(want) {
		t.Fatalf("supportedToolNames(exec) len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedToolNames(exec)[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestSupportedToolNames_MapsCommonOpenClawAliases(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "read_files"},
		map[string]interface{}{"name": "write"},
		map[string]interface{}{"name": "edit"},
		map[string]interface{}{"name": "shell"},
		map[string]interface{}{"name": "glob"},
		map[string]interface{}{"name": "grep"},
		map[string]interface{}{"name": "sessions_spawn"},
		map[string]interface{}{"name": "use_skill"},
		map[string]interface{}{"name": "process"},
		map[string]interface{}{"name": "browser"},
	}

	got := supportedToolNames(tools)
	want := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", "Task", "Skill"}
	if len(got) != len(want) {
		t.Fatalf("supportedToolNames(common aliases) len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedToolNames(common aliases)[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}
