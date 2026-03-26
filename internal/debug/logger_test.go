package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerLogInputTokenBreakdownWritesFile(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	defer func() {
		_ = os.Chdir(wd)
	}()

	logger := New(true, false)
	if logger == nil || logger.Dir() == "" {
		t.Fatal("expected enabled logger with directory")
	}

	logger.LogInputTokenBreakdown("bolt", 101, 202, 303, 404, 1010)

	path := filepath.Join(logger.Dir(), "6_input_token_breakdown.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}

	content := string(raw)
	for _, want := range []string{
		`"prompt_profile": "bolt"`,
		`"base_prompt_tokens": 101`,
		`"system_context_tokens": 202`,
		`"history_tokens": 303`,
		`"tools_tokens": 404`,
		`"estimated_total": 1010`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("breakdown file missing %q, content=%s", want, content)
		}
	}
}
