package handler

import (
	"strings"
	"testing"
)

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

// The estimate is what a client budgets against, so it has to describe the
// request this gateway actually sends. The previous implementation measured a
// compacted projection (at most 24 tools, 128-character descriptions, 4 KiB
// schemas) that was never forwarded, which under-reported the input.
func TestEstimateToolsTokensMeasuresTheToolsThatAreSent(t *testing.T) {
	tools := sampleIncomingTools()
	if got := estimateToolsTokens(tools); got <= 0 {
		t.Fatalf("estimateToolsTokens() = %d, want a positive estimate", got)
	}
	// A tool the old projection would have dropped must still change the
	// estimate, otherwise the number does not describe the request.
	withExtra := append(append([]interface{}{}, tools...), map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "Agent",
			"description": strings.Repeat("long description ", 200),
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{"type": "string", "description": strings.Repeat("payload ", 200)},
				},
			},
		},
	})
	if estimateToolsTokens(withExtra) <= estimateToolsTokens(tools) {
		t.Fatal("a tool the projection would have dropped did not change the estimate")
	}
}

func TestEstimateToolsTokensIsEmptyForNoTools(t *testing.T) {
	if got := estimateToolsTokens(nil); got != 0 {
		t.Fatalf("estimateToolsTokens(nil) = %d, want 0", got)
	}
	if got := estimateToolsTokens([]interface{}{}); got != 0 {
		t.Fatalf("estimateToolsTokens(empty) = %d, want 0", got)
	}
}

// A tool description the old code cut to 128 characters still counts in full.
func TestEstimateToolsTokensCountsLongDescriptionsInFull(t *testing.T) {
	short := []interface{}{map[string]interface{}{
		"name":        "Bash",
		"description": "run a command",
	}}
	long := []interface{}{map[string]interface{}{
		"name":        "Bash",
		"description": strings.Repeat("run a command with a great deal of extra guidance ", 40),
	}}
	if estimateToolsTokens(long) <= estimateToolsTokens(short) {
		t.Fatal("a long description must count in full, not be capped at 128 characters")
	}
}

func BenchmarkEstimateToolsTokens(b *testing.B) {
	tools := sampleIncomingTools()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = estimateToolsTokens(tools)
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
