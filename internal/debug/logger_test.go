package debug

import (
	"os"
	"path/filepath"
	"runtime"
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
	if logger == nil || logger.dir == "" {
		t.Fatal("expected enabled logger with directory")
	}

	logger.LogInputTokenBreakdown("workbuddy", 101, 202, 303, 404, 1010)

	path := filepath.Join(logger.dir, "6_input_token_breakdown.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}

	content := string(raw)
	for _, want := range []string{
		`"prompt_profile": "workbuddy"`,
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

func TestLoggerLogUpstreamRequestRedactsCredentials(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(%q) error = %v", tmp, err)
	}
	defer func() { _ = os.Chdir(wd) }()

	logger := New(true, false)
	logger.LogUpstreamRequest("https://example.test", map[string]string{
		"Authorization": "Bearer secret-jwt",
		"Cookie":        "session=secret-cookie",
		"Content-Type":  "application/json",
	}, map[string]any{"ok": true})

	path := filepath.Join(logger.dir, "3_upstream_request.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	content := string(raw)
	if strings.Contains(content, "secret-jwt") || strings.Contains(content, "secret-cookie") {
		t.Fatalf("credential leaked into debug log: %s", content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Fatalf("expected redaction marker: %s", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	// Windows does not expose Unix permission bits through os.FileMode.
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0600 {
		t.Fatalf("debug log permissions=%o want 600", got)
	}
}
