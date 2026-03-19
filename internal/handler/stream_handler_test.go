package handler

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
)

type flushRecorder struct {
	header  http.Header
	buf     bytes.Buffer
	code    int
	flushes int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: make(http.Header), code: 200}
}

func (r *flushRecorder) Header() http.Header         { return r.header }
func (r *flushRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
func (r *flushRecorder) WriteHeader(statusCode int)  { r.code = statusCode }
func (r *flushRecorder) Flush()                      { r.flushes++ }

func TestMarshalSSEPayloads_ManualJSONEscapes(t *testing.T) {
	newline := string(byte('\n'))
	expectedText := "he" + "\"" + "llo" + newline + "next"
	raw, err := marshalSSEContentBlockDeltaText(7, expectedText)
	if err != nil {
		t.Fatalf("marshal text delta: %v", err)
	}
	var delta map[string]any
	if err := json.Unmarshal([]byte(raw), &delta); err != nil {
		t.Fatalf("unmarshal text delta: %v", err)
	}
	if int(delta["index"].(float64)) != 7 {
		t.Fatalf("unexpected index: %v", delta["index"])
	}
	deltaObj := delta["delta"].(map[string]any)
	if deltaObj["type"] != "text_delta" || deltaObj["text"] != expectedText {
		t.Fatalf("unexpected delta payload: %#v", deltaObj)
	}

	expectedToolID := `tool_"1`
	expectedToolName := "Wr" + newline + "ite"
	raw, err = marshalSSEContentBlockStartToolUse(3, expectedToolID, expectedToolName)
	if err != nil {
		t.Fatalf("marshal tool start: %v", err)
	}
	var startPayload map[string]any
	if err := json.Unmarshal([]byte(raw), &startPayload); err != nil {
		t.Fatalf("unmarshal tool start: %v", err)
	}
	contentBlock := startPayload["content_block"].(map[string]any)
	if contentBlock["id"] != expectedToolID || contentBlock["name"] != expectedToolName {
		t.Fatalf("unexpected tool payload: %#v", contentBlock)
	}

	expectedSignature := "sig\"" + newline + "next"
	rawBytes, err := marshalSSEContentBlockStartThinkingBytes(4, expectedSignature)
	if err != nil {
		t.Fatalf("marshal thinking start: %v", err)
	}
	var thinkingStartPayload map[string]any
	if err := json.Unmarshal(rawBytes, &thinkingStartPayload); err != nil {
		t.Fatalf("unmarshal thinking start: %v", err)
	}
	thinkingBlock := thinkingStartPayload["content_block"].(map[string]any)
	if thinkingBlock["type"] != "thinking" || thinkingBlock["signature"] != expectedSignature {
		t.Fatalf("unexpected thinking payload: %#v", thinkingBlock)
	}

	expectedPartialJSON := "{\"path\":\"a.txt\",\"content\":\"he\\\"llo" + newline + "next\"}"
	rawBytes, err = marshalSSEContentBlockDeltaInputJSONBytes(5, expectedPartialJSON)
	if err != nil {
		t.Fatalf("marshal input_json delta: %v", err)
	}
	var inputJSONPayload map[string]any
	if err := json.Unmarshal(rawBytes, &inputJSONPayload); err != nil {
		t.Fatalf("unmarshal input_json delta: %v", err)
	}
	inputDelta := inputJSONPayload["delta"].(map[string]any)
	if inputDelta["type"] != "input_json_delta" || inputDelta["partial_json"] != expectedPartialJSON {
		t.Fatalf("unexpected input_json payload: %#v", inputDelta)
	}

	expectedStopReason := "tool_\"use" + newline + "next"
	rawBytes, err = marshalSSEMessageDeltaBytes(expectedStopReason, 42)
	if err != nil {
		t.Fatalf("marshal message delta: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(rawBytes, &msg); err != nil {
		t.Fatalf("unmarshal message delta: %v", err)
	}
	if msg["type"] != "message_delta" {
		t.Fatalf("unexpected message type: %#v", msg)
	}
	if msg["delta"].(map[string]any)["stop_reason"] != expectedStopReason {
		t.Fatalf("unexpected stop reason: %#v", msg)
	}
	if int(msg["usage"].(map[string]any)["output_tokens"].(float64)) != 42 {
		t.Fatalf("unexpected output tokens: %#v", msg)
	}

	msgStartRaw, err := marshalSSEMessageStartBytes("msg_123", "claude-test", 12, 0)
	if err != nil {
		t.Fatalf("marshal message start: %v", err)
	}
	var msgStart map[string]any
	if err := json.Unmarshal(msgStartRaw, &msgStart); err != nil {
		t.Fatalf("unmarshal message start: %v", err)
	}
	if msgStart["type"] != "message_start" {
		t.Fatalf("unexpected message start type: %#v", msgStart["type"])
	}
	messageObj := msgStart["message"].(map[string]any)
	if messageObj["id"] != "msg_123" || messageObj["model"] != "claude-test" {
		t.Fatalf("unexpected message object: %#v", messageObj)
	}
	usageObj := messageObj["usage"].(map[string]any)
	if int(usageObj["input_tokens"].(float64)) != 12 || int(usageObj["output_tokens"].(float64)) != 0 {
		t.Fatalf("unexpected usage object: %#v", usageObj)
	}

	msgStartNoUsageRaw, err := marshalSSEMessageStartNoUsageBytes("dup", "claude-test")
	if err != nil {
		t.Fatalf("marshal message start no usage: %v", err)
	}
	var msgStartNoUsage map[string]any
	if err := json.Unmarshal(msgStartNoUsageRaw, &msgStartNoUsage); err != nil {
		t.Fatalf("unmarshal message start no usage: %v", err)
	}
	messageNoUsageObj := msgStartNoUsage["message"].(map[string]any)
	if _, ok := messageNoUsageObj["usage"]; ok {
		t.Fatalf("expected no usage field, got: %#v", messageNoUsageObj)
	}

	plainText := "hello ??"
	rawBytes, err = marshalSSEContentBlockDeltaTextBytes(9, plainText)
	if err != nil {
		t.Fatalf("marshal plain text delta: %v", err)
	}
	var plainDelta map[string]any
	if err := json.Unmarshal(rawBytes, &plainDelta); err != nil {
		t.Fatalf("unmarshal plain text delta: %v", err)
	}
	if got := plainDelta["delta"].(map[string]any)["text"]; got != plainText {
		t.Fatalf("unexpected plain text delta: %#v", got)
	}

	htmlEscaped := "<tag>&\u2028\u2029"
	rawBytes, err = marshalSSEContentBlockDeltaTextBytes(10, htmlEscaped)
	if err != nil {
		t.Fatalf("marshal html escaped delta: %v", err)
	}
	if !bytes.Contains(rawBytes, []byte("\\u003c")) || !bytes.Contains(rawBytes, []byte("\\u003e")) || !bytes.Contains(rawBytes, []byte("\\u0026")) {
		t.Fatalf("expected html-sensitive bytes to be escaped, got: %s", rawBytes)
	}
	if !bytes.Contains(rawBytes, []byte("\\u2028")) || !bytes.Contains(rawBytes, []byte("\\u2029")) {
		t.Fatalf("expected line separator bytes to be escaped, got: %s", rawBytes)
	}
	var escapedDelta map[string]any
	if err := json.Unmarshal(rawBytes, &escapedDelta); err != nil {
		t.Fatalf("unmarshal html escaped delta: %v", err)
	}
	if got := escapedDelta["delta"].(map[string]any)["text"]; got != htmlEscaped {
		t.Fatalf("unexpected escaped text delta: %#v", got)
	}
}

func TestAppendSSEPayloadBuildersMatchMarshal(t *testing.T) {
	tests := []struct {
		name     string
		marshal  func() ([]byte, error)
		appendTo func([]byte) ([]byte, error)
	}{
		{
			name:    "tool start",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockStartToolUseBytes(3, `tool_"1`, "Wr\nite") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStartToolUse(dst, 3, `tool_"1`, "Wr\nite")
			},
		},
		{
			name:    "text start",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockStartTextBytes(4) },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStartText(dst, 4)
			},
		},
		{
			name:    "thinking start",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockStartThinkingBytes(5, "sig\n123") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStartThinking(dst, 5, "sig\n123")
			},
		},
		{
			name: "input json delta",
			marshal: func() ([]byte, error) {
				return marshalSSEContentBlockDeltaInputJSONBytes(6, `{"path":"a.txt","content":"he\"llo"}`)
			},
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaInputJSON(dst, 6, `{"path":"a.txt","content":"he\"llo"}`)
			},
		},
		{
			name:    "text delta",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockDeltaTextBytes(7, "hello\nworld") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaText(dst, 7, "hello\nworld")
			},
		},
		{
			name:    "thinking delta",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockDeltaThinkingBytes(8, "step <1>") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaThinking(dst, 8, "step <1>")
			},
		},
		{
			name:    "block stop",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockStopBytes(9) },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStop(dst, 9)
			},
		},
		{
			name:    "message delta",
			marshal: func() ([]byte, error) { return marshalSSEMessageDeltaBytes("tool_use\nnext", 42) },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEMessageDelta(dst, "tool_use\nnext", 42)
			},
		},
	}

	buf := make([]byte, 0, 256)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := tt.marshal()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := tt.appendTo(buf[:0])
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got=%s want=%s", got, want)
			}
			buf = got[:0]
		})
	}
}

func TestSanitizeToolInput_FieldMapping(t *testing.T) {
	in := `{"path":"a.txt","content":"hi","overwrite":true}`
	out := sanitizeToolInput("write", in)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("expected json out: %v", err)
	}
	if _, ok := m["overwrite"]; ok {
		t.Fatalf("expected overwrite removed")
	}
	if m["file_path"] != "a.txt" {
		t.Fatalf("expected file_path mapped, got %v", m["file_path"])
	}
	if _, ok := m["path"]; ok {
		t.Fatalf("expected path removed")
	}
}

func TestNormalizeUpstreamToolCall_ListDirUsesTopLevelBash(t *testing.T) {
	name, input := normalizeUpstreamToolCall("LS", `{"path":"/tmp/project"}`, "/Users/dailin/Documents/GitHub/TEST")
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("expected json input, got %v", err)
	}
	if payload["command"] != `ls -1A -- "/tmp/project"` {
		t.Fatalf("expected top-level ls command, got %s", payload["command"])
	}
	if payload["description"] != "List top-level directory entries" {
		t.Fatalf("expected directory list description, got %s", payload["description"])
	}
}

func TestNormalizeUpstreamToolCall_ListDirPlaceholderPathFallsBackToWorkdir(t *testing.T) {
	workdir := "/Users/dailin/Documents/GitHub/TEST"
	name, input := normalizeUpstreamToolCall("LS", `{"path":"/home/user/app"}`, workdir)
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("expected json input, got %v", err)
	}
	if payload["command"] != `ls -1A -- "`+workdir+`"` {
		t.Fatalf("expected workdir fallback command, got %s", payload["command"])
	}
}

func TestNormalizeUpstreamToolCall_GlobPreservesGlob(t *testing.T) {
	name, input := normalizeUpstreamToolCall("Glob", `{"path":"/tmp/project"}`, "/Users/dailin/Documents/GitHub/TEST")
	if name != "Glob" {
		t.Fatalf("expected Glob, got %q", name)
	}
	if !strings.Contains(input, `"pattern":"*"`) {
		t.Fatalf("expected glob pattern injection, got %s", input)
	}
}

func TestNormalizeUpstreamToolCall_RebasesForeignReadPathToWorkdir(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "dashboard_data.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Read", `{"file_path":"/Users/junchaoli/monitor_trump/dashboard_data.json"}`, workdir)
	if name != "Read" {
		t.Fatalf("expected Read, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if payload["file_path"] != target {
		t.Fatalf("expected foreign path to rebase to %q, got %q", target, payload["file_path"])
	}
}

func TestNormalizeUpstreamToolCall_RebasesForeignGlobPathToWorkdir(t *testing.T) {
	workdir := t.TempDir()
	targetDir := filepath.Join(workdir, "web-ui")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Glob", `{"path":"/Users/dailin/dev/caption-cloud/web-ui","pattern":"*"}`, workdir)
	if name != "Glob" {
		t.Fatalf("expected Glob, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if payload["path"] != targetDir {
		t.Fatalf("expected foreign glob path to rebase to %q, got %q", targetDir, payload["path"])
	}
}

func TestNormalizeUpstreamToolCall_RebasesForeignWritePathToWorkdirWhenFileMissing(t *testing.T) {
	workdir := t.TempDir()

	name, input := normalizeUpstreamToolCall("Write", `{"file_path":"/tmp/cc-agent/sb1-fxjxbmvk/project/calculator.py","content":"print('ok')"}`, workdir)
	if name != "Write" {
		t.Fatalf("expected Write, got %q", name)
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	want := filepath.Join(workdir, "calculator.py")
	if payload["file_path"] != want {
		t.Fatalf("expected foreign write path to rebase to %q, got %q", want, payload["file_path"])
	}
	if payload["content"] != "print('ok')" {
		t.Fatalf("expected content preserved, got %q", payload["content"])
	}
	if strings.Contains(payload["file_path"], "/tmp/cc-agent/") {
		t.Fatalf("expected placeholder write path removed, got %q", payload["file_path"])
	}
}

func TestNormalizeUpstreamToolCall_RebasesForeignNestedWritePathWhenLocalParentExists(t *testing.T) {
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Write", `{"file_path":"/tmp/cc-agent/sb1-fxjxbmvk/project/src/calculator.py","content":"print('ok')"}`, workdir)
	if name != "Write" {
		t.Fatalf("expected Write, got %q", name)
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	want := filepath.Join(workdir, "src", "calculator.py")
	if payload["file_path"] != want {
		t.Fatalf("expected nested foreign write path to rebase to %q, got %q", want, payload["file_path"])
	}
}

func TestNormalizeUpstreamToolCall_ForeignAbsoluteReadFallsBackToBashWhenWorkdirUnknown(t *testing.T) {
	name, input := normalizeUpstreamToolCall("Read", `{"file_path":"/Users/jianxinwei/workspace/cursor-monitor/README.md"}`, "/home/dailin")
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	if !strings.Contains(input, "README.md") {
		t.Fatalf("expected basename candidate in bash fallback, got %s", input)
	}
	if !strings.Contains(input, "sed -n '1,240p'") {
		t.Fatalf("expected bash read fallback command, got %s", input)
	}
	if strings.Contains(input, " -- ") {
		t.Fatalf("expected BSD-compatible sed fallback without --, got %s", input)
	}
}

func TestNormalizeUpstreamToolCall_RewritesForeignBashReadCandidatesToWorkdir(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "dashboard_data.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Bash", `{"command":"if [ -f \"jinyaozhang/Projects/truth_social_scraper/dashboard_data.json\" ]; then sed -n '1,240p' < \"jinyaozhang/Projects/truth_social_scraper/dashboard_data.json\"; exit 0; fi; if [ -f \"dashboard_data.json\" ]; then sed -n '1,240p' < \"dashboard_data.json\"; exit 0; fi"}`, workdir)
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	normalizedCommand := strings.ToLower(strings.ReplaceAll(payload["command"], `\`, `/`))
	normalizedTarget := strings.ToLower(strings.ReplaceAll(target, `\`, `/`))
	for strings.Contains(normalizedCommand, "//") {
		normalizedCommand = strings.ReplaceAll(normalizedCommand, "//", "/")
	}
	if !strings.Contains(normalizedCommand, normalizedTarget) {
		t.Fatalf("expected bash read candidate to rebase to %q, got %s", target, payload["command"])
	}
	if strings.Contains(input, "jinyaozhang/Projects/truth_social_scraper") {
		t.Fatalf("expected foreign path to be removed from localized bash command, got %s", input)
	}
}

func TestNormalizeUpstreamToolCall_RewritesForeignRelativeReadTailToWorkdir(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "test_caption_cloud.py")
	if err := os.WriteFile(target, []byte("print('ok')"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Bash", `{"command":"if [ -f \"jianxinwei/workspace/monitor_trump/test_caption_cloud.py\" ]; then sed -n '1,240p' < \"jianxinwei/workspace/monitor_trump/test_caption_cloud.py\"; exit 0; fi"}`, workdir)
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	normalizedCommand := strings.ToLower(strings.ReplaceAll(payload["command"], `\`, `/`))
	normalizedTarget := strings.ToLower(strings.ReplaceAll(target, `\`, `/`))
	for strings.Contains(normalizedCommand, "//") {
		normalizedCommand = strings.ReplaceAll(normalizedCommand, "//", "/")
	}
	if !strings.Contains(normalizedCommand, normalizedTarget) {
		t.Fatalf("expected relative foreign path to rebase to %q, got %s", target, payload["command"])
	}
	if strings.Contains(input, "jianxinwei/workspace/monitor_trump") {
		t.Fatalf("expected foreign path to be removed from localized bash command, got %s", input)
	}
}

func TestNormalizeUpstreamToolCall_LocalizesForeignBashReadCandidatesToFindInWorkdir(t *testing.T) {
	workdir := t.TempDir()
	nested := filepath.Join(workdir, "src", "dashboard_data.json")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(nested, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	name, input := normalizeUpstreamToolCall("Bash", `{"command":"if [ -f \"jinyaozhang/Projects/truth_social_scraper/dashboard_data.json\" ]; then sed -n '1,240p' < \"jinyaozhang/Projects/truth_social_scraper/dashboard_data.json\"; exit 0; fi; if [ -f \"truth_social_scraper/dashboard_data.json\" ]; then sed -n '1,240p' < \"truth_social_scraper/dashboard_data.json\"; exit 0; fi"}`, workdir)
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if !strings.Contains(payload["command"], "find . -type f -name \"dashboard_data.json\"") {
		t.Fatalf("expected localized find-based lookup, got %s", input)
	}
	if strings.Contains(input, "jinyaozhang/Projects/truth_social_scraper") {
		t.Fatalf("expected foreign path to be removed, got %s", input)
	}
}

func TestNormalizeUpstreamToolCall_RewritesProjectRootProbeCommandToRelativeList(t *testing.T) {
	workdir := `d:\Code\Orchids-2api`
	name, input := normalizeUpstreamToolCall("Bash", `{"command":"ls /mnt/d/Code/Orchids-2api 2>/dev/null || ls ~/Orchids-2api 2>/dev/null || echo \"cannot access windows path\"","description":"Try to access Windows project path"}`, workdir)
	if name != "Bash" {
		t.Fatalf("expected Bash, got %q", name)
	}

	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("expected json input, got %v", err)
	}
	if payload["command"] != `ls -1A -- "."` {
		t.Fatalf("expected relative root list command, got %s", payload["command"])
	}
	if payload["description"] != "List project root directory entries" {
		t.Fatalf("expected localized description, got %s", payload["description"])
	}
	if strings.Contains(input, "/mnt/d/Code/Orchids-2api") || strings.Contains(input, "~/Orchids-2api") {
		t.Fatalf("expected foreign probe paths removed, got %s", input)
	}
}

func TestHasRequiredToolInput_Validations(t *testing.T) {
	if hasRequiredToolInput("write", `{}`) {
		t.Fatalf("write should require path+content")
	}
	if !hasRequiredToolInput("write", `{"file_path":"a","content":"x"}`) {
		t.Fatalf("write with file_path+content should be valid")
	}
	if !hasRequiredToolInput("write", `{"path":"a","content":"x"}`) {
		t.Fatalf("write with legacy path should be valid")
	}
	if hasRequiredToolInput("bash", `{"cmd":""}`) {
		t.Fatalf("bash should require non-empty cmd/command")
	}
}

func TestSideEffectToolDedupKey(t *testing.T) {
	if got := sideEffectToolDedupKey("bash", `{"command":"echo 1"}`); got != "bash:echo 1" {
		t.Fatalf("unexpected key: %q", got)
	}
	if got := sideEffectToolDedupKey("write", `{"file_path":"a","content":"x"}`); !strings.HasPrefix(got, "write:a\x00") {
		t.Fatalf("unexpected key: %q", got)
	}
	if got := sideEffectToolDedupKey("read", `{"file_path":"a"}`); got != "" {
		t.Fatalf("read should not be treated as side effect")
	}
}

func TestNormalizeIntroKey(t *testing.T) {
	if got := normalizeIntroKey("  Hello! How can I help you today? "); got != "intro:en:greet" {
		t.Fatalf("unexpected: %q", got)
	}
	if got := normalizeIntroKey("Hi! What's up? How can I help today?"); got != "intro:en:greet" {
		t.Fatalf("unexpected english variant: %q", got)
	}
	if got := normalizeIntroKey("你好，我能帮你什么"); got != "intro:zh:greet" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestCollapseDuplicatedIntroDelta(t *testing.T) {
	in := "Hi! What's up? How can I help today?Hi! What's up? How can I help today?"
	out := collapseDuplicatedIntroDelta(in)
	if out != "Hi! What's up? How can I help today?" {
		t.Fatalf("unexpected collapse result: %q", out)
	}
}

func TestStreamHandler_TextFlow_AnthropicSSE(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	// seed a message_start so the stream resembles real output
	sh.writeSSE("message_start", `{"type":"message_start"}`)

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-start"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hi"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-end"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}})

	out := rec.buf.String()
	if !strings.Contains(out, "event: content_block_start") {
		t.Fatalf("expected content_block_start in output, got: %s", out)
	}
	if !strings.Contains(out, "\"text\":\"hi\"") {
		t.Fatalf("expected text delta, got: %s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected message_stop, got: %s", out)
	}
}

func TestStreamHandler_ToolInput_EndEmitsToolUse(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-start", "id": "t1", "toolName": "bash"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-delta", "id": "t1", "delta": `{"command":"echo 1"}`}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-end", "id": "t1"}})

	out := rec.buf.String()
	if !strings.Contains(out, "\"type\":\"tool_use\"") {
		t.Fatalf("expected tool_use emitted, got: %s", out)
	}
	if !strings.Contains(out, "echo 1") {
		t.Fatalf("expected command in tool input, got: %s", out)
	}
}

func TestStreamHandler_ListDirToolInput_EndEmitsBashTopLevelList(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	workdir := "/Users/dailin/Documents/GitHub/TEST"
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, workdir)
	defer sh.release()

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-start", "id": "t1", "toolName": "LS"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-delta", "id": "t1", "delta": `{"path":"/home/user/app"}`}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tool-input-end", "id": "t1"}})

	out := rec.buf.String()
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("expected LS to emit Bash tool_use, got: %s", out)
	}
	if !strings.Contains(out, `ls -1A -- \\\"`+workdir+`\\\"`) {
		t.Fatalf("expected workdir top-level ls command, got: %s", out)
	}
}

func TestStreamHandler_OpenAI_SendsDONEOnStop(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatOpenAI, "")
	defer sh.release()

	sh.finishResponse("end_turn")
	out := rec.buf.String()
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("expected [DONE] for openai SSE, got: %s", out)
	}
}

func TestWriteSSEFrameBytes_Output(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSSEFrameBytes(&buf, "content_block_delta", []byte("{\"type\":\"content_block_delta\"}")); err != nil {
		t.Fatalf("writeSSEFrameBytes: %v", err)
	}
	got := buf.String()
	want := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	if got != want {
		t.Fatalf("unexpected SSE frame output: %q", got)
	}
}

func TestWriteOpenAIFrame_Output(t *testing.T) {
	var buf bytes.Buffer
	if err := writeOpenAIFrame(&buf, []byte("{\"id\":\"msg_1\"}")); err != nil {
		t.Fatalf("writeOpenAIFrame: %v", err)
	}
	got := buf.String()
	want := "data: {\"id\":\"msg_1\"}\n\n"
	if got != want {
		t.Fatalf("unexpected OpenAI frame output: %q", got)
	}
}

func TestStreamHandler_WriteChunkFallback_EmitsTextBlock(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.writeChunkBuffer.WriteString("fallback hello")
	sh.finishResponse("end_turn")

	out := rec.buf.String()
	if !strings.Contains(out, "event: content_block_start") {
		t.Fatalf("expected content_block_start, got: %s", out)
	}
	if !strings.Contains(out, `"type":"text"`) {
		t.Fatalf("expected text content block, got: %s", out)
	}
	if !strings.Contains(out, `"text":"fallback hello"`) {
		t.Fatalf("expected fallback text delta, got: %s", out)
	}
	if !strings.Contains(out, "event: content_block_stop") {
		t.Fatalf("expected content_block_stop, got: %s", out)
	}
}

func TestMaskDedupKey_Stable(t *testing.T) {
	cfg := &config.Config{}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	a := sh.maskDedupKey("bash:echo 1")
	b := sh.maskDedupKey("bash:echo 1")
	if a != b {
		t.Fatalf("expected stable mask")
	}
	if !strings.HasPrefix(a, "bash#") {
		t.Fatalf("expected prefix bash#, got %q", a)
	}
}

func (h *streamHandler) maskDedupKey(key string) string { return maskDedupKey(key) }

func TestExtractThinkingSignature(t *testing.T) {
	e := map[string]any{"signature": "sig"}
	if got := extractThinkingSignature(e); got != "sig" {
		t.Fatalf("unexpected: %q", got)
	}
	e2 := map[string]any{"data": map[string]any{"signature": "sig2"}}
	if got := extractThinkingSignature(e2); got != "sig2" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestStreamHandler_TokensUsed_OverridesEstimation(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, OutputTokenMode: "final"}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setUsageTokens(10, -1)
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "tokens-used", "inputTokens": float64(12), "outputTokens": float64(34)}})

	// finishing should keep upstream usage (useUpstreamUsage=true)
	sh.finishResponse("end_turn")
	if sh.inputTokens != 12 || sh.outputTokens != 34 {
		t.Fatalf("unexpected usage: in=%d out=%d", sh.inputTokens, sh.outputTokens)
	}
}

func TestStreamHandler_FinalOutputTokens_MatchChunkedText(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, OutputTokenMode: "final"}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-start"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hel"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "lo world!"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-end"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}})

	want := tiktoken.EstimateTextTokens("hello world!")
	if sh.outputTokens != want {
		t.Fatalf("unexpected output tokens: got=%d want=%d", sh.outputTokens, want)
	}
}

func TestStreamHandler_KeepAlive_NoPanic(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	// should not write once hasReturn set
	sh.hasReturn = true
	sh.writeKeepAlive()
	if rec.buf.Len() != 0 {
		t.Fatalf("expected no output when hasReturn")
	}

	// reset and ensure it writes
	sh.hasReturn = false
	sh.writeKeepAlive()
	if !strings.Contains(rec.buf.String(), ": keep-alive") {
		t.Fatalf("expected keep-alive comment")
	}
}

func TestStreamHandler_EventThrottle_fs_operation(t *testing.T) {
	cfg := &config.Config{DebugEnabled: true}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.handleMessage(upstream.SSEMessage{Type: "fs_operation", Event: map[string]any{"operation": "scan"}})
	first := rec.buf.Len()
	sh.handleMessage(upstream.SSEMessage{Type: "fs_operation", Event: map[string]any{"operation": "scan"}})
	second := rec.buf.Len()
	if second != first {
		t.Fatalf("expected throttling to suppress second fs_operation within 1s")
	}
	// allow after 1s
	sh.lastScanTime = time.Now().Add(-2 * time.Second)
	sh.handleMessage(upstream.SSEMessage{Type: "fs_operation", Event: map[string]any{"operation": "scan"}})
	if rec.buf.Len() == second {
		t.Fatalf("expected third fs_operation to be written after throttle window")
	}
}

func TestStreamHandler_CoalescesNonTextFlushes(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.writeSSE("message_start", `{"type":"message_start"}`)
	if rec.flushes != 1 {
		t.Fatalf("expected message_start to flush immediately, got %d", rec.flushes)
	}

	thinkingData, err := marshalSSEContentBlockDeltaThinking(0, "step")
	if err != nil {
		t.Fatalf("marshal thinking delta: %v", err)
	}
	for i := 0; i < sseDeferredFlushFrameThreshold-1; i++ {
		sh.writeSSE("content_block_delta", thinkingData)
	}
	if rec.flushes != 1 {
		t.Fatalf("expected deferred thinking deltas to coalesce, got %d flushes", rec.flushes)
	}
	sh.writeSSE("content_block_delta", thinkingData)
	if rec.flushes != 2 {
		t.Fatalf("expected deferred threshold flush, got %d", rec.flushes)
	}

	textData, err := marshalSSEContentBlockDeltaText(0, "hi")
	if err != nil {
		t.Fatalf("marshal text delta: %v", err)
	}
	sh.writeSSE("content_block_delta", textData)
	if rec.flushes != 3 {
		t.Fatalf("expected text delta to flush immediately, got %d", rec.flushes)
	}
}

func TestStreamHandler_CoalescesNonTextFlushes_Bytes(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.writeSSEBytes("message_start", []byte(`{"type":"message_start"}`))
	if rec.flushes != 1 {
		t.Fatalf("expected message_start to flush immediately, got %d", rec.flushes)
	}

	thinkingData, err := marshalSSEContentBlockDeltaThinkingBytes(0, "step")
	if err != nil {
		t.Fatalf("marshal thinking delta: %v", err)
	}
	for i := 0; i < sseDeferredFlushFrameThreshold-1; i++ {
		sh.writeSSEBytes("content_block_delta", thinkingData)
	}
	if rec.flushes != 1 {
		t.Fatalf("expected deferred thinking deltas to coalesce, got %d flushes", rec.flushes)
	}
	sh.writeSSEBytes("content_block_delta", thinkingData)
	if rec.flushes != 2 {
		t.Fatalf("expected deferred threshold flush, got %d", rec.flushes)
	}

	textData, err := marshalSSEContentBlockDeltaTextBytes(0, "hi")
	if err != nil {
		t.Fatalf("marshal text delta: %v", err)
	}
	sh.writeSSEBytes("content_block_delta", textData)
	if rec.flushes != 3 {
		t.Fatalf("expected text delta to flush immediately, got %d", rec.flushes)
	}
}

func TestStreamHandler_FinishResponse_InjectsNoToolsFallbackWhenOnlySuppressedToolsRemain(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setNoToolsFallbackText("当前只拿到目录概览，还不足以继续分析。")
	sh.suppressedToolCalls = 1
	sh.finishResponse("end_turn")

	out := rec.buf.String()
	if !strings.Contains(out, "当前只拿到目录概览，还不足以继续分析。") {
		t.Fatalf("expected no-tools fallback text in SSE output, got: %s", out)
	}
	if !strings.Contains(out, `"output_tokens":`) {
		t.Fatalf("expected message_delta usage, got: %s", out)
	}
}

func TestStreamHandler_FinishResponse_SuppressesGenericEmptyFallbackWhenRequested(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setSuppressEmptyOutputFallback(true)
	sh.finishResponse("end_turn")

	out := rec.buf.String()
	if strings.Contains(out, "No output was presented to the user") {
		t.Fatalf("expected generic empty fallback to stay suppressed, got: %s", out)
	}
	if !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected message_stop even when empty fallback is suppressed, got: %s", out)
	}
}

func TestStreamHandler_FinishResponse_UsesPriorToolResultFallbackForSuppressedDuplicateMutations(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setPreferPriorToolResultFallback(true)
	sh.toolDedupCount = 1
	sh.suppressedToolCalls = 1
	sh.finishResponse("end_turn")

	out := rec.buf.String()
	if !strings.Contains(out, duplicateToolResultFallbackText) {
		t.Fatalf("expected duplicate-tool fallback text in SSE output, got: %s", out)
	}
	if strings.Contains(out, genericEmptyOutputFallbackText) {
		t.Fatalf("did not expect generic empty fallback when duplicate-tool fallback is preferred, got: %s", out)
	}
}

func TestStreamHandler_FinishResponse_InjectsNoToolsFallbackDespiteInternalBlocks(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setNoToolsFallbackText("当前已根据已有文件内容给出本地结论。")
	sh.suppressedToolCalls = 2
	sh.contentBlocks = append(sh.contentBlocks, map[string]any{"type": "thinking", "text": "internal"})
	sh.finishResponse("end_turn")

	out := rec.buf.String()
	if !strings.Contains(out, "当前已根据已有文件内容给出本地结论。") {
		t.Fatalf("expected no-tools fallback text in SSE output, got: %s", out)
	}
}

func TestStreamHandler_FinishResponse_AppendsNoToolsFallbackAfterWeakPreface(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setNoToolsFallbackText("从当前已读取内容看，这是一个 Python 项目，包含 Flask、urllib 和本地 JSON 文件。")
	sh.suppressedToolCalls = 1
	sh.hasTextOutput = true
	sh.responseText.WriteString("Let me explore the project structure to understand the technical architecture.")
	sh.contentBlocks = append(sh.contentBlocks, map[string]any{
		"type": "text",
		"text": "Let me explore the project structure to understand the technical architecture.",
	})

	sh.finishResponse("end_turn")

	if !strings.Contains(sh.responseText.String(), "Flask") {
		t.Fatalf("expected appended no-tools fallback, got: %s", sh.responseText.String())
	}
}

func TestStreamHandler_FinishResponse_AppendsNoToolsFallbackAfterWeakPrefaceWithoutSuppressedTools(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setNoToolsFallbackText("基于当前已读取内容，优先可做这几项优化：统一超时与错误处理；抽离数据访问层；补最小测试。")
	sh.hasTextOutput = true
	sh.responseText.WriteString("让我先了解一下项目的结构和代码。")
	sh.contentBlocks = append(sh.contentBlocks, map[string]any{
		"type": "text",
		"text": "让我先了解一下项目的结构和代码。",
	})

	sh.finishResponse("end_turn")

	if !strings.Contains(sh.responseText.String(), "统一超时与错误处理") {
		t.Fatalf("expected appended no-tools fallback without suppressed tools, got: %s", sh.responseText.String())
	}
}

func TestStreamHandler_FinishResponse_AppendsNoToolsFallbackAfterExaminePrefaceWithoutSuppressedTools(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, false, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setNoToolsFallbackText("从当前已读取内容看，可维护性风险主要在：同一模块同时承担网络、存储和业务职责，分层边界不清。")
	sh.hasTextOutput = true
	sh.responseText.WriteString("Let me examine the project structure and code to identify maintainability risks.")
	sh.contentBlocks = append(sh.contentBlocks, map[string]any{
		"type": "text",
		"text": "Let me examine the project structure and code to identify maintainability risks.",
	})

	sh.finishResponse("end_turn")

	if !strings.Contains(sh.responseText.String(), "可维护性风险") {
		t.Fatalf("expected appended no-tools fallback for examine preface, got: %s", sh.responseText.String())
	}
}

func TestStreamHandler_NoToolsGateSuppressesValidToolCall(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false}
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(cfg, rec, logger, false, true, adapter.FormatAnthropic, "")
	defer sh.release()

	sh.setDisallowToolCalls(true)
	sh.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]any{
			"type":       "tool-call",
			"toolCallId": "tool_1",
			"toolName":   "Read",
			"input":      `{"file_path":"README.md"}`,
		},
	})
	sh.finishResponse("tool_use")

	out := rec.buf.String()
	if strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("expected tool_use to be suppressed, got: %s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("expected end_turn after suppressing tool call, got: %s", out)
	}
}
