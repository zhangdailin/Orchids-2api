package orchids

import (
	"strings"
	"testing"
)

func TestBuildLocalAssistantPrompt_ContainsSingleResultGuideline(t *testing.T) {
	t.Parallel()

	prompt := buildLocalAssistantPrompt("", "hello", "", "")
	if !strings.Contains(prompt, "工具执行成功后只输出一次简短结果") {
		t.Fatalf("expected Chinese single-result guideline to be present")
	}
	if !strings.Contains(prompt, "After tool success, emit one concise completion message only") {
		t.Fatalf("expected English single-result guideline to be present")
	}
	if !strings.Contains(prompt, "删除命令遇到“no matches found / No such file or directory”时视为幂等无操作") {
		t.Fatalf("expected Chinese no-op delete guideline to be present")
	}
	if !strings.Contains(prompt, "treat as idempotent no-op and do not rerun the same delete command") {
		t.Fatalf("expected English no-op delete guideline to be present")
	}
	if !strings.Contains(prompt, "命令出现交互输入错误（如 EOFError: EOF when reading a line）时，不要重复执行同一命令") {
		t.Fatalf("expected Chinese EOFError no-rerun guideline to be present")
	}
	if !strings.Contains(prompt, "If a command fails with interactive stdin errors") {
		t.Fatalf("expected English EOFError no-rerun guideline to be present")
	}
}
