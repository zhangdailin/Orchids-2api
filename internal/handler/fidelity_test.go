package handler

import (
	"testing"
)

// Fidelity 保真回归：网关必须原样透传系统内容，不改写 cc_entrypoint 等标记。

func TestSanitizeSystemItems_AlwaysPreservesVerbatim(t *testing.T) {
	system := SystemItems{
		{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.85.351; cc_entrypoint=cli; cch=5e896;"},
		{Type: "text", Text: "cc_entrypoint=claude-code; keep=this"},
		{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
		{Type: "text", Text: "# Environment\n - Primary working directory: C:\\work\n\ngitStatus:\n?? file.txt"},
	}

	got, changed := sanitizeSystemItems(system)
	if changed {
		t.Fatal("fidelity must never change system items")
	}
	if len(got) != len(system) {
		t.Fatalf("len=%d want=%d", len(got), len(system))
	}
	for i := range system {
		if got[i].Text != system[i].Text || got[i].Type != system[i].Type {
			t.Fatalf("system[%d] rewritten: %#v", i, got[i])
		}
	}
}
