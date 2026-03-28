package bolt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestSendRequestWithPayload_EmitsModelEvents(t *testing.T) {
	prevURL := boltAPIURL
	prevRootURL := boltRootDataURL
	t.Cleanup(func() {
		boltAPIURL = prevURL
		boltRootDataURL = prevRootURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"projectId":"sb1-demo"`) {
			t.Fatalf("request body missing projectId: %s", string(body))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:\"hello\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []string
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg.Type)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	want := []string{"model.text-delta", "model.finish"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want %v", events, want)
	}
}

func TestSendRequestWithPayload_IgnoresNoReplySentinel(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:\"NO_REPLY\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("events len=%d want 1, events=%#v", len(events), events)
	}
	if events[0].Type != "model.finish" {
		t.Fatalf("first event type=%q want model.finish", events[0].Type)
	}
}

func TestSendRequestWithPayload_ConvertsJSONToolCallTextToModelToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	toolJSON, err := json.Marshal(`{"tool":"Read","parameters":{"file_path":"README.md"}}`)
	if err != nil {
		t.Fatalf("marshal tool json: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(toolJSON)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "read readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
	if got := events[0].Event["input"]; got != `{"file_path":"README.md"}` {
		t.Fatalf("input=%v want read input json", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
}

func TestSendRequestWithPayload_ConvertsLeadingJSONToolCallsWithTrailingSummary(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk, err := json.Marshal("{\n  \"tool_calls\": [\n    {\n      \"tool\": \"Edit\",\n      \"parameters\": {\n        \"file_path\": \"/tmp/cc-agent/sb1-sg78wfbc/project/calculator.py\",\n        \"old_string\": \"return a + b\",\n        \"new_string\": \"return add(a, b)\"\n      }\n    }\n  ]\n}\n\n完成！已添加科学计算功能。")
	if err != nil {
		t.Fatalf("marshal mixed chunk: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计算功能"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Edit" {
		t.Fatalf("toolName=%v want Edit", got)
	}
	input, ok := events[0].Event["input"].(string)
	if !ok {
		t.Fatalf("input=%T want string", events[0].Event["input"])
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if payload["file_path"] != "/tmp/cc-agent/sb1-sg78wfbc/project/calculator.py" {
		t.Fatalf("file_path=%q want sandbox calculator path", payload["file_path"])
	}
	if payload["old_string"] != "return a + b" {
		t.Fatalf("old_string=%q want original code", payload["old_string"])
	}
	if payload["new_string"] != "return add(a, b)" {
		t.Fatalf("new_string=%q want replacement code", payload["new_string"])
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
}

func TestSendRequestWithPayload_FlushesUnclosedJSONCodeFenceAsToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk, err := json.Marshal("```json\n{\"tool_calls\":[{\"function\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}]}")
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
}

func TestSendRequestWithPayload_DropsNarrationBeforeToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	preamble, err := json.Marshal("看起来这些工具结果是从你本地环境回传的。现在直接执行提交：")
	if err != nil {
		t.Fatalf("marshal preamble: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(preamble)+"\n")
		_, _ = io.WriteString(w, "9:{\"toolName\":\"Bash\",\"args\":{\"command\":\"git add -A && git commit -m \\\"Update bolt client and config\\\" && git push\",\"description\":\"Stage, commit and push all changes\"}}\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Bash" {
		t.Fatalf("toolName=%v want Bash", got)
	}
	if got := events[0].Event["input"]; got != `{"command":"git add -A \u0026\u0026 git commit -m \"Update bolt client and config\" \u0026\u0026 git push","description":"Stage, commit and push all changes"}` {
		t.Fatalf("input=%v want bash git input", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_StopsAfterFirstStructuredToolCall(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "9:{\"toolName\":\"Bash\",\"args\":{\"command\":\"git status --short\",\"description\":\"Check git status\"}}\n")
		_, _ = io.WriteString(w, "a:{\"toolCallId\":\"toolu_1\",\"toolName\":\"Bash\",\"args\":{\"command\":\"git status --short\"},\"result\":{\"type\":\"error\",\"content\":\"fatal: not a git repository\"}}\n")
		_, _ = io.WriteString(w, "0:\"please run git init manually\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
		_, _ = io.WriteString(w, "d:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Bash" {
		t.Fatalf("toolName=%v want Bash", got)
	}
	if got := events[0].Event["input"]; got != `{"command":"git status --short","description":"Check git status"}` {
		t.Fatalf("input=%v want git status tool input", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_PreservesMarkdownCodeFenceLanguageAcrossChunks(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk1, err := json.Marshal("计算器已创建，运行方式：\n```")
	if err != nil {
		t.Fatalf("marshal chunk1: %v", err)
	}
	chunk2, err := json.Marshal("bash\npython calculator.py\n```")
	if err != nil {
		t.Fatalf("marshal chunk2: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk1)+"\n")
		_, _ = io.WriteString(w, "0:"+string(chunk2)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "写一个计算器"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("events len=%d want 3", len(events))
	}
	if events[0].Type != "model.text-delta" || events[1].Type != "model.text-delta" {
		t.Fatalf("unexpected event types: %v, %v", events[0].Type, events[1].Type)
	}
	if events[2].Type != "model.finish" {
		t.Fatalf("third event type=%q want model.finish", events[2].Type)
	}
	got := events[0].Event["delta"].(string) + events[1].Event["delta"].(string)
	want := "计算器已创建，运行方式：\n```bash\npython calculator.py\n```"
	if got != want {
		t.Fatalf("delta=%q want %q", got, want)
	}
}

func TestSendRequestWithPayload_StripsBoltToolTranscriptFromFinalText(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	chunk, err := json.Marshal("Read 1 file (ctrl+o to expand)\n\n● Write(calculator.py)\n  ⎿  Wrote 42 lines to calculator.py\n       1 def add(a, b): return a + b\n\n● 计算器已创建，运行方式：\n```bash\npython calculator.py\n```")
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err = client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "写一个计算器"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("events len=%d want 3", len(events))
	}
	if events[0].Type != "model.text-delta" || events[1].Type != "model.text-delta" {
		t.Fatalf("unexpected event types: %v, %v", events[0].Type, events[1].Type)
	}
	if events[2].Type != "model.finish" {
		t.Fatalf("third event type=%q want model.finish", events[2].Type)
	}
	got := events[0].Event["delta"].(string) + events[1].Event["delta"].(string)
	if strings.Contains(got, "ctrl+o to expand") || strings.Contains(got, "Write(calculator.py)") || strings.Contains(got, "Wrote 42 lines") {
		t.Fatalf("delta still contains tool transcript: %q", got)
	}
	want := "计算器已创建，运行方式：\n```bash\npython calculator.py\n```"
	if got != want {
		t.Fatalf("delta=%q want %q", got, want)
	}
}

func TestSendRequestWithPayload_HandlesBoltToolInvocationFramesAndFinalDMarker(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "9:{\"toolCallId\":\"toolu_1\",\"toolName\":\"Bash\",\"args\":{\"command\":\"ls /tmp/cc-agent/sb1-demo/project/\",\"description\":\"List project files\"}}\n")
		_, _ = io.WriteString(w, "a:{\"toolCallId\":\"toolu_1\",\"toolName\":\"Bash\",\"args\":{\"command\":\"ls /tmp/cc-agent/sb1-demo/project/\",\"description\":\"List project files\"},\"result\":\"README.md\"}\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"unknown\",\"isContinued\":false,\"usage\":{\"promptTokens\":3,\"completionTokens\":27}}\n")
		_, _ = io.WriteString(w, "0:\"沙箱中\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"isContinued\":false,\"usage\":{\"promptTokens\":5,\"completionTokens\":386}}\n")
		_, _ = io.WriteString(w, "d:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":386}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.text-delta", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Bash" {
		t.Fatalf("toolName=%v want Bash", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_UsesPreparedInputEstimateInFinishUsage(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "0:\"hello\"\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "keep this custom instruction"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
		},
	}

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	want := EstimateInputTokens(req)
	var events []upstream.SSEMessage
	if err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	usage, ok := events[1].Event["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("finish usage=%#v", events[1].Event["usage"])
	}
	if got := usage["inputTokens"]; got != want.Total {
		t.Fatalf("inputTokens=%v want %d", got, want.Total)
	}
}

func TestSendRequestWithPayload_StreamsVisibleTextBeforeUpstreamCompletes(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "0:\"hello\"\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
		_, _ = io.WriteString(w, "d:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":7}}\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "demo",
	}, nil)

	gotFirstVisible := make(chan upstream.SSEMessage, 1)
	done := make(chan error, 1)
	go func() {
		done <- client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
			Model: "claude-opus-4-6",
			Messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
			},
		}, func(msg upstream.SSEMessage) {
			if msg.Type == "model.text-delta" {
				select {
				case gotFirstVisible <- msg:
				default:
				}
			}
		}, nil)
	}()

	select {
	case msg := <-gotFirstVisible:
		if got := msg.Event["delta"]; got != "hello" {
			t.Fatalf("delta=%v want hello", got)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("expected visible text to stream before upstream completed")
	}

	if err := <-done; err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}
}

func TestSendRequestWithPayload_RetriesFalseCompletionAfterFailedEdit(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			chunk, _ := json.Marshal("科学计算器已更新完成，新增以下功能：科学计数法、sin、cos、log。")
			_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Read\",\"args\":{\"file_path\":\"calculator.py\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_edit",
						Name:  "Edit",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_edit",
						Content:   "<tool_use_error>String to replace not found in file.</tool_use_error>",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count=%d want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 上次修改仍失败") {
		t.Fatalf("second request missing false-completion correction, body=%s", requestBodies[1])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_RetriesFalseCompletionAfterReadOnlyFollowup(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			chunk, _ := json.Marshal("上一轮已经完成了科学计算功能的添加，新版 calculator.py 已包含科学记数法输入。")
			_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Edit\",\"args\":{\"file_path\":\"calculator.py\",\"old_string\":\"return a + b\",\"new_string\":\"return add(a, b)\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_write",
						Name:  "Write",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_write",
						Content:   "File created successfully at: calculator.py",
					}},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n3→\n4→def subtract(a, b):\n",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count=%d want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 上次只有 Read、没有新的成功 Write/Edit，不能算完成") {
		t.Fatalf("second request missing read-only false-completion correction, body=%s", requestBodies[1])
	}
	if !strings.Contains(requestBodies[1], "calculator.py") {
		t.Fatalf("second request missing read path hint, body=%s", requestBodies[1])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Edit" {
		t.Fatalf("toolName=%v want Edit", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_RetriesFalseCompletionAfterRepeatedReadCorrection(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Read\",\"args\":{\"file_path\":\"calculator.py\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		case 1:
			chunk, _ := json.Marshal("我没有再次读取 calculator.py。刚才的请求是创建该文件，已成功完成。")
			_, _ = io.WriteString(w, "0:"+string(chunk)+"\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":9}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Edit\",\"args\":{\"file_path\":\"calculator.py\",\"old_string\":\"return a + b\",\"new_string\":\"return add_scientific(a, b)\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_glob",
						Name:  "Glob",
						Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_glob",
						Content:   "calculator.py",
					}},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 3 {
		t.Fatalf("request count=%d want 3", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 刚读过 `calculator.py`") {
		t.Fatalf("second request missing repeated-read correction, body=%s", requestBodies[1])
	}
	if !strings.Contains(requestBodies[2], "不要把旧成功或文件存在当成这次已完成") {
		t.Fatalf("third request missing create-success false-completion correction, body=%s", requestBodies[2])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if got := events[0].Event["toolName"]; got != "Edit" {
		t.Fatalf("toolName=%v want Edit", got)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_HidesRootProjectProbeAfterFailedMutation(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Glob\",\"args\":{\"path\":\".\",\"pattern\":\"*.py\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Write\",\"args\":{\"file_path\":\"calculator.py\",\"content\":\"import math\\nprint(math.pi)\\n\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→import ast\n2→\n3→def add(a, b):\n4→    return a + b\n",
					}},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_edit",
						Name:  "Edit",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_edit",
						Content:   "<tool_use_error>String to replace not found in file.\nString: import ast\n</tool_use_error>",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count=%d want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 别再对根目录做 Glob/Grep 探路") {
		t.Fatalf("second request missing root-probe correction, body=%s", requestBodies[1])
	}
	if !strings.Contains(requestBodies[1], "若 Edit 命不中，就改用 Write") {
		t.Fatalf("second request missing write fallback correction, body=%s", requestBodies[1])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Write" {
		t.Fatalf("toolName=%v want Write", got)
	}
	if events[1].Type != "model.finish" {
		t.Fatalf("second event type=%q want model.finish", events[1].Type)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_RetriesRepeatedReadAfterReadOnlyFollowup(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Read\",\"args\":{\"file_path\":\"calculator.py\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Edit\",\"args\":{\"file_path\":\"calculator.py\",\"old_string\":\"return a + b\",\"new_string\":\"return parse_number(a) + parse_number(b)\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n3→\n4→def subtract(a, b):\n",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count=%d want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 刚读过 `calculator.py`") {
		t.Fatalf("second request missing repeated-read correction, body=%s", requestBodies[1])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if got := events[0].Event["toolName"]; got != "Edit" {
		t.Fatalf("toolName=%v want Edit", got)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_RetriesEmptyTurnAfterReadOnlyFollowup(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	var requestBodies []string
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBodies = append(requestBodies, string(body))
		w.WriteHeader(http.StatusOK)
		switch attempt {
		case 0:
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"end_turn\",\"usage\":{\"promptTokens\":5,\"completionTokens\":0}}\n")
		default:
			_, _ = io.WriteString(w, "9:{\"toolName\":\"Edit\",\"args\":{\"file_path\":\"calculator.py\",\"old_string\":\"return a + b\",\"new_string\":\"return add(a, b)\"}}\n")
			_, _ = io.WriteString(w, "e:{\"finishReason\":\"tool_use\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
		}
		attempt++
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
		},
	}

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), req, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count=%d want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[1], "RETRY: 已拿到 `calculator.py` 的内容，不要空结束") {
		t.Fatalf("second request missing empty-turn correction, body=%s", requestBodies[1])
	}
	if len(events) != 2 {
		t.Fatalf("events len=%d want 2, events=%v", len(events), events)
	}
	if got := events[0].Event["toolName"]; got != "Edit" {
		t.Fatalf("toolName=%v want Edit", got)
	}
	if got := events[1].Event["finishReason"]; got != "tool_use" {
		t.Fatalf("finishReason=%v want tool_use", got)
	}
}

func TestSendRequestWithPayload_ParsesStructuredToolCallEnvelope(t *testing.T) {
	prevURL := boltAPIURL
	t.Cleanup(func() { boltAPIURL = prevURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "9:{\"tool_calls\":[{\"function\":\"Read\",\"parameters\":{\"file_path\":\"README.md\"}}]}\n")
		_, _ = io.WriteString(w, "e:{\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":5,\"completionTokens\":3}}\n")
	}))
	defer srv.Close()
	boltAPIURL = srv.URL

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect readme"}},
		},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	if err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events len=%d want 2", len(events))
	}
	if events[0].Type != "model.tool-call" {
		t.Fatalf("first event type=%q want model.tool-call", events[0].Type)
	}
	if got := events[0].Event["toolName"]; got != "Read" {
		t.Fatalf("toolName=%v want Read", got)
	}
	if got := events[0].Event["input"]; got != `{"file_path":"README.md"}` {
		t.Fatalf("input=%v want read input json", got)
	}
}

func TestEstimateInputTokens_SplitsPromptBuckets(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "keep this custom instruction"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "inspect project"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "I will inspect the repository."}},
		},
	}

	got := EstimateInputTokens(req)
	if got.BasePromptTokens <= 0 {
		t.Fatalf("BasePromptTokens=%d want >0", got.BasePromptTokens)
	}
	if got.ToolsTokens <= 0 {
		t.Fatalf("ToolsTokens=%d want >0", got.ToolsTokens)
	}
	if got.SystemContextTokens <= 0 {
		t.Fatalf("SystemContextTokens=%d want >0", got.SystemContextTokens)
	}
	if got.HistoryTokens <= 0 {
		t.Fatalf("HistoryTokens=%d want >0", got.HistoryTokens)
	}
	if got.Total != got.BasePromptTokens+got.SystemContextTokens+got.ToolsTokens+got.HistoryTokens {
		t.Fatalf("Total=%d does not match bucket sum", got.Total)
	}
}

func TestEstimateInputTokens_AggressivelyCompactsLongFocusedReadHistory(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我给 calculator.py 添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   strings.Repeat("1234567890", 1200),
					}},
				},
			},
		},
	}

	got := EstimateInputTokens(req)
	if got.HistoryTokens <= 0 {
		t.Fatalf("HistoryTokens=%d want >0", got.HistoryTokens)
	}
	if got.HistoryTokens >= 1200 {
		t.Fatalf("HistoryTokens=%d want aggressive compaction below 1200", got.HistoryTokens)
	}
}

func TestPrepareRequest_AddsWorkspaceAndToolInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"function": map[string]interface{}{"name": "Bash"}},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "keep this custom instruction"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "当前项目目录名: Orchids-2api") {
		t.Fatalf("system prompt missing project name hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "当前项目真实工作目录") || !strings.Contains(boltReq.GlobalSystemPrompt, "`d:\\Code\\Orchids-2api`") {
		t.Fatalf("system prompt missing explicit workdir hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要回答 `/tmp/cc-agent/...`") {
		t.Fatalf("system prompt missing sandbox-path answer guard: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "把项目根目录视为 `.`") {
		t.Fatalf("system prompt missing relative root instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "当前项目已经是一个 git 仓库") {
		t.Fatalf("system prompt missing git repository hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Read(file_path, limit?, offset?)") {
		t.Fatalf("system prompt missing Read tool hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要先解释计划") {
		t.Fatalf("system prompt missing direct tool-call instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要解释当前运行在什么系统或沙箱") {
		t.Fatalf("system prompt missing no-sandbox-explanation instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "若当前问题需要的能力并不在上面的工具列表里，直接说明限制") {
		t.Fatalf("system prompt missing unavailable-capability guard: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "如果 Write/Edit 的工具结果出现 `Hook PreToolUse` 或 `denied this tool`") {
		t.Fatalf("system prompt should avoid coding-only mutation guidance when request is not a code-edit task: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(strings.ToLower(boltReq.GlobalSystemPrompt), "anthropic's official cli for claude") {
		t.Fatalf("system prompt should strip claude code system boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "Primary working directory") {
		t.Fatalf("system prompt should strip raw environment workdir block: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "keep this custom instruction") {
		t.Fatalf("system prompt dropped custom instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_DoesNotAdvertiseBoltToolsWhenRequestOmitsTools(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加 科学计数法"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	for _, marker := range []string{
		"Read(file_path, limit?, offset?)",
		"Write(file_path, content)",
		"Edit(file_path, old_string, new_string, replace_all?)",
		"Bash(command, description?, timeout?, run_in_background?)",
		"Glob(path, pattern)",
		"Grep(path, pattern",
	} {
		if strings.Contains(boltReq.GlobalSystemPrompt, marker) {
			t.Fatalf("system prompt should not advertise bolt tools when request omitted tools; found marker %q in %s", marker, boltReq.GlobalSystemPrompt)
		}
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "TodoWrite(content)") {
		t.Fatalf("system prompt should not advertise TodoWrite by default: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_DropsMCPBoilerplateButKeepsUsefulContext(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: strings.Join([]string{
				"You are an interactive agent that helps users with software engineering tasks.",
				"IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts.",
				"# System",
				"- All text you output outside of tool use is displayed to the user.",
				"# Using your tools",
				"- Do NOT use the Bash to run commands when a relevant dedicated tool is provided.",
				"# MCP Server Instructions",
				"## context7",
				"Use this server to retrieve up-to-date documentation and code examples for any library.",
				"gitStatus: This is the git status at the start of the conversation.",
				"Current branch: main",
				"Status:",
				"M internal/bolt/client.go",
				"Recent commits:",
				"0823630 Improve Grok, Bolt, and Warp Request Handling",
				"# VSCode Extension Context",
				"keep this custom instruction",
			}, "\n")},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if strings.Contains(strings.ToLower(boltReq.GlobalSystemPrompt), "anthropic's official cli for claude") {
		t.Fatalf("system prompt should strip claude code boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "# MCP Server") {
		t.Fatalf("system prompt should drop MCP heading boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "## context7") {
		t.Fatalf("system prompt should drop MCP server name boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "Use this server to retrieve up-to-date documentation and code examples for any library.") {
		t.Fatalf("system prompt should drop MCP instruction boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "Do NOT use the Bash to run commands") {
		t.Fatalf("system prompt should drop claude code tool boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "Assist with authorized security testing") {
		t.Fatalf("system prompt should drop claude code preamble boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "gitStatus: This is the git status at the start of the conversation.") {
		t.Fatalf("system prompt should preserve git snapshot context: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Current branch: main") {
		t.Fatalf("system prompt should preserve current branch context: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Recent commits:") {
		t.Fatalf("system prompt should preserve recent commit context: %s", boltReq.GlobalSystemPrompt)
	}
	if strings.Contains(boltReq.GlobalSystemPrompt, "# VSCode Extension Context") {
		t.Fatalf("system prompt should drop VSCode heading boilerplate: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "keep this custom instruction") {
		t.Fatalf("system prompt should preserve custom instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_AddsGitExecutionInstructionsForUploadIntent(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "这已经构成对本地 git add、git commit、git push 的明确授权") {
		t.Fatalf("system prompt missing git execution authorization hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "第一步优先直接使用 Bash 执行 git status 或 git status --short") {
		t.Fatalf("system prompt missing git-status-first hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "默认指当前工作区里的全部改动") {
		t.Fatalf("system prompt missing default-all-changes hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要只给用户输出命令步骤") {
		t.Fatalf("system prompt missing do-not-dump-git-commands hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要重新打招呼") {
		t.Fatalf("system prompt missing continue-without-greeting hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要再根据 `/tmp/cc-agent/...`") {
		t.Fatalf("system prompt missing no-sandbox-git-misdiagnosis hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要重复已经成功完成的同一步") {
		t.Fatalf("system prompt missing no-repeat-successful-step hint: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_AddsEditFollowupExecutionInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Write"},
			map[string]interface{}{"name": "Edit"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要声称“已经完成”") {
		t.Fatalf("system prompt missing no-false-completion hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要停在现状总结") {
		t.Fatalf("system prompt missing direct-edit follow-up hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "用户后续补充的技术说明、约束或示例") {
		t.Fatalf("system prompt missing continuation-spec hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要仅为了确认结果就再次 Read 同一文件") {
		t.Fatalf("system prompt missing post-write no-reread hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "优先使用 Edit 做最小修改") {
		t.Fatalf("system prompt missing prefer-edit hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "第一个非空输出字符应当直接是 `{`") {
		t.Fatalf("system prompt missing direct-json-first hint: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestBuildBoltWorkspacePrompt_IncludesGitRepoHint(t *testing.T) {
	workdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(workdir, ".git"), 0o755); err != nil {
		t.Fatalf("failed to create fake .git dir: %v", err)
	}
	prompt := strings.Join(buildBoltWorkspacePrompt(workdir), "\n")
	if !strings.Contains(prompt, "当前项目已经是一个 git 仓库") {
		t.Fatalf("workspace prompt missing git repository hint: %s", prompt)
	}
}

func TestPrepareRequest_AddsHistoryAwarePathRecoveryInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Bash"},
			map[string]interface{}{"name": "Glob"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_ls",
							Name:  "Bash",
							Input: map[string]interface{}{"command": "ls /tmp/cc-agent/sb1-demo/project", "description": "List project files"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_ls",
							Content:   "Exit code 2\nls: cannot access '/tmp/cc-agent/sb1-demo/project': No such file or directory",
						},
						{Type: "text", Text: "这个项目是干什么的"},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "`/tmp/cc-agent/sb1-demo/project`") {
		t.Fatalf("system prompt missing invalid-history path hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "真实项目目录是 `d:\\Code\\Orchids-2api`") {
		t.Fatalf("system prompt missing explicit real-workdir recovery hint: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "不要复用这个路径") {
		t.Fatalf("system prompt missing do-not-reuse instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "在至少成功查看一次 `.`、README.md、go.mod、package.json 等项目内路径之前") {
		t.Fatalf("system prompt missing must-check-project instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_AddsEmptyProjectDirectCreateInstructions(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: `C:\Users\zhangdailin\Desktop\新建文件夹 (2)`,
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Write"},
			map[string]interface{}{"name": "Bash"},
			map[string]interface{}{"name": "Glob"},
		},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "若刚通过 Glob/Read/Bash 确认项目根目录为空") {
		t.Fatalf("system prompt missing empty-project direct-create instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "优先直接使用 Write 创建首个文件") {
		t.Fatalf("system prompt missing empty-project write-first instruction: %s", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "默认直接在项目根目录创建 `calculator.py`") {
		t.Fatalf("system prompt missing python-calculator default file instruction: %s", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_TrimsSupersededEmptyProjectClarificationHistory(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "随便写点东西"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Text: "这是一个空项目。请告诉我你想要构建什么？",
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob",
							Content:   "No files found",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "帮我用python写一个计算器" {
		t.Fatalf("first message content=%q want latest concrete request", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "Tool result:\nNo files found") {
		t.Fatalf("second message content=%q want latest empty-project tool result", got)
	}
	for _, msg := range boltReq.Messages {
		if strings.Contains(msg.Content, "这是一个空项目") || strings.Contains(msg.Content, "随便写点东西") {
			t.Fatalf("expected stale empty-project clarification history to be trimmed, got messages=%#v", boltReq.Messages)
		}
	}
}

func TestPrepareRequest_DropsMisleadingNoFilesFoundProbeAfterSuccessfulWrite(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py", "content": "print(1)"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "File created successfully at: calculator.py",
						},
					},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*.{js,ts,tsx,jsx,vue}"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob",
							Content:   "No files found",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	for _, msg := range boltReq.Messages {
		if strings.Contains(msg.Content, "No files found") {
			t.Fatalf("expected misleading no-files-found probe to be trimmed, got messages=%#v", boltReq.Messages)
		}
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "帮我添加科学计数法") {
		t.Fatalf("last message content=%q want follow-up edit request", got)
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "这是新的修改请求") {
		t.Fatalf("last message content=%q want fresh-modification guard", got)
	}
}

func TestPrepareRequest_DropsSupersededNoFilesFoundProbeAfterLaterPositiveGlob(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "C:\\Users\\zhangdailin\\Desktop\\1212",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob_frontend",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*.{js,jsx,ts,tsx,vue}"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob_frontend",
							Content:   "No files found",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob_all",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob_all",
							Content:   "C:\\Users\\zhangdailin\\Desktop\\1212\\calculator.py",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "No files found") {
		t.Fatalf("expected superseded empty probe to be trimmed, got: %q", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "calculator.py") {
		t.Fatalf("expected later positive glob result to remain, got: %q", got)
	}
}

func TestPrepareRequest_DropsPositiveProjectProbeAfterLaterRead(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob",
							Content:   "calculator.py",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→def add(a, b):\n2→    return a + b\n",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "Tool result:\ncalculator.py") {
		t.Fatalf("expected superseded positive probe to be trimmed after later read, got: %q", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "1→def add(a, b):") {
		t.Fatalf("expected later read result to remain, got: %q", got)
	}
}

func TestPrepareRequest_DropsRedundantPositiveProjectProbeAfterEarlierRead(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "给 calculator.py 添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_glob",
						Name:  "Glob",
						Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_glob",
						Content:   "calculator.py",
					}},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "Tool result:\ncalculator.py") {
		t.Fatalf("expected redundant positive probe after earlier read to be trimmed, got: %q", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "1→def add(a, b):") {
		t.Fatalf("expected earlier read result to remain, got: %q", got)
	}
}

func TestPrepareRequest_EncodesToolResultsAsUserContentAndDropsAssistantToolInvocations(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "README.md"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "# Orchids-2api\n一个基于 Go 的多通道代理服务。",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	if got := boltReq.Messages[1].Role; got != "user" {
		t.Fatalf("last role=%q want user", got)
	}
	if !strings.Contains(boltReq.Messages[1].Content, "Tool result:") {
		t.Fatalf("expected tool result to be serialized into user content, got: %q", boltReq.Messages[1].Content)
	}
	if strings.Contains(boltReq.Messages[1].Content, "toolInvocation") {
		t.Fatalf("did not expect tool invocation metadata in user content, got: %q", boltReq.Messages[1].Content)
	}
	if len(boltReq.Messages[0].Parts) != 0 {
		t.Fatalf("expected first user message to have no parts, got: %#v", boltReq.Messages[0].Parts)
	}
	if len(boltReq.Messages[1].Parts) != 0 {
		t.Fatalf("expected tool-result follow-up user message to have no parts, got: %#v", boltReq.Messages[1].Parts)
	}
}

func TestPrepareRequest_PreservesMultiTurnEditHistoryAfterWrite(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py", "content": "print(1)"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "File created successfully at: calculator.py",
						},
					},
				},
			},
			{Role: "assistant", Content: prompt.MessageContent{Text: "完成！计算器已创建在项目目录中。"}},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 4 {
		t.Fatalf("messages len=%d want 4, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "帮我用python写一个计算器" {
		t.Fatalf("first message content=%q want original create request", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "只做最小确认") {
		t.Fatalf("second message content=%q want minimal confirmation after write success", got)
	}
	if got := boltReq.Messages[2].Content; got != "完成！计算器已创建在项目目录中。" {
		t.Fatalf("third message content=%q want assistant completion", got)
	}
	if got := boltReq.Messages[3].Content; got != "帮我添加科学计数法" {
		t.Fatalf("fourth message content=%q want follow-up edit request", got)
	}
}

func TestPrepareRequest_DropsSupersededAssistantCompletionSummaryBeforeLaterEditFollowup(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py", "content": "print(1)"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "File created successfully at: calculator.py",
						},
					},
				},
			},
			{Role: "assistant", Content: prompt.MessageContent{Text: "计算器已创建完成，文件为 `calculator.py`。运行方式：python calculator.py。支持加、减、乘、除，输入 quit 退出。"}},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→def add(a, b):\n2→    return a + b\n",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	for _, msg := range boltReq.Messages {
		if strings.Contains(msg.Content, "计算器已创建完成") {
			t.Fatalf("expected stale assistant completion summary to be trimmed, got messages=%#v", boltReq.Messages)
		}
	}
	if got := boltReq.Messages[1].Content; got != "帮我添加科学计数法" {
		t.Fatalf("second message content=%q want latest explicit edit request", got)
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "下面是上一轮工具结果") {
		t.Fatalf("third message content=%q want neutral tool-result marker for follow-up", got)
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "def add(a, b)") {
		t.Fatalf("third message content=%q want latest read result", got)
	}
}

func TestPrepareRequest_DropsInjectedFailureAssistantNoiseBeforeLaterBoltFollowup(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮他添加科学计数法"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "Request failed: all available accounts for this channel are currently rate-limited. Please wait for cooldown or add another valid account. (selector: no enabled accounts available for channel: bolt, last error: bolt API error: status=429, body={\"code\":\"rate-limited\"})"}},
			{Role: "user", Content: prompt.MessageContent{Text: "给他添加科学计数法"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "Request failed: all available accounts for this channel are currently rate-limited. Please wait for cooldown or add another valid account. (selector: no enabled accounts available for channel: bolt, last error: bolt API error: status=429, body={\"code\":\"rate-limited\"})"}},
			{Role: "user", Content: prompt.MessageContent{Text: "给他添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 4 {
		t.Fatalf("messages len=%d want 4, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	for _, msg := range boltReq.Messages {
		if strings.Contains(msg.Content, "all available accounts for this channel are currently rate-limited") {
			t.Fatalf("expected injected failure assistant text to be trimmed, got messages=%#v", boltReq.Messages)
		}
	}
	if got := boltReq.Messages[0].Content; got != "帮他添加科学计数法" {
		t.Fatalf("first message content=%q want original task", got)
	}
	if got := boltReq.Messages[1].Content; got != "给他添加科学计数法" {
		t.Fatalf("second message content=%q want first retry task", got)
	}
	if got := boltReq.Messages[2].Content; got != "给他添加科学计数法" {
		t.Fatalf("third message content=%q want latest retry task", got)
	}
	if got := boltReq.Messages[3].Content; !strings.Contains(got, "下面是上一轮工具结果") {
		t.Fatalf("fourth message content=%q want neutral tool-result marker for read follow-up", got)
	}
	if got := boltReq.Messages[3].Content; !strings.Contains(got, "def add(a, b)") {
		t.Fatalf("fourth message content=%q want read result", got)
	}
}

func TestPrepareRequest_DropsUnsupportedSimToolResultNoise(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_git",
							Name:  "Bash",
							Input: map[string]interface{}{"command": "git status --short"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_git",
							Content:   "M internal/bolt/client.go",
						},
						{
							Type:      "tool_result",
							ToolUseID: "tool_git",
							Content:   "UNSUPPORTED_SIM_COMMAND: ",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	if strings.Contains(boltReq.Messages[1].Content, "UNSUPPORTED_SIM_COMMAND") {
		t.Fatalf("expected unsupported sim tool result to be dropped, got: %q", boltReq.Messages[1].Content)
	}
	if !strings.Contains(boltReq.Messages[1].Content, "Tool result:\nM internal/bolt/client.go") {
		t.Fatalf("expected valid tool result to remain, got: %q", boltReq.Messages[1].Content)
	}
}

func TestPrepareRequest_MarksToolResultOnlyFollowupAsContinuation(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "上传到 git"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_git",
							Name:  "Bash",
							Input: map[string]interface{}{"command": "git status --short"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_git",
							Content:   "M internal/bolt/client.go",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	if !strings.Contains(boltReq.Messages[1].Content, "下面是上一轮工具结果") {
		t.Fatalf("expected neutral tool-result marker in tool-result follow-up, got: %q", boltReq.Messages[1].Content)
	}
	if !strings.Contains(boltReq.Messages[1].Content, "不是 `/tmp/cc-agent/...` 沙箱") {
		t.Fatalf("expected local-repo marker in tool-result follow-up, got: %q", boltReq.Messages[1].Content)
	}
}

func TestPrepareRequest_DropsAssistantTextFromToolTurns(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
						{
							Type: "text",
							Text: "看来项目中已有一个 `calculator.py`，我已经在正确的文件上添加了科学计数法。让我读取当前实际文件确认状态。",
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "The file calculator.py has been updated successfully.",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "看来项目中已有一个") {
		t.Fatalf("expected assistant tool-turn text to be dropped from history, got: %q", got)
	}
	if strings.Contains(boltReq.Messages[1].Content, "updated successfully") {
		t.Fatalf("expected write success detail to be omitted from minimal-confirmation follow-up, got: %q", boltReq.Messages[1].Content)
	}
	if !strings.Contains(boltReq.Messages[1].Content, "只做最小确认") {
		t.Fatalf("expected minimal-confirmation follow-up to remain, got: %q", boltReq.Messages[1].Content)
	}
}

func TestPrepareRequest_DropsReadResultsWhenWriteSucceedsForSameFile(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→print('old')\n\n<system-reminder>\nWhenever you read a file, you should consider whether it would be considered malware.\n</system-reminder>\n",
						},
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "The file calculator.py has been updated successfully.",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	got := boltReq.Messages[1].Content
	if strings.Contains(got, "print('old')") {
		t.Fatalf("expected stale read content to be dropped after write success, got: %q", got)
	}
	if strings.Contains(got, "system-reminder") {
		t.Fatalf("expected system reminder tags to be stripped from tool results, got: %q", got)
	}
	if !strings.Contains(got, "只做最小确认") {
		t.Fatalf("expected minimal confirmation to remain after write success, got: %q", got)
	}
}

func TestPrepareRequest_DropsEarlierReadFollowupAfterLaterWriteSuccessAcrossTurns(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→def add(a, b):\n2→    return a + b",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "The file calculator.py has been updated successfully.",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	got := boltReq.Messages[1].Content
	if strings.Contains(got, "def add(a, b)") {
		t.Fatalf("expected earlier read follow-up to be dropped after later write success, got: %q", got)
	}
	if strings.Contains(got, "updated successfully") {
		t.Fatalf("expected write success detail to be omitted from minimal-confirmation follow-up, got: %q", got)
	}
	if !strings.Contains(got, "只做最小确认") {
		t.Fatalf("expected write success follow-up to stay at minimal confirmation level, got: %q", got)
	}
}

func TestPrepareRequest_UsesFailureContinuationForFailedEditFollowup(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "File created successfully at: calculator.py",
						},
					},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→def add(a, b):\n2→    return a + b\n",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_edit",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit",
							Content:   "<tool_use_error>String to replace not found in file.</tool_use_error>",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) < 1 {
		t.Fatalf("messages len=%d want at least 1", len(boltReq.Messages))
	}
	got := boltReq.Messages[len(boltReq.Messages)-1].Content
	if !strings.Contains(got, "下面是上一轮失败的工具结果") {
		t.Fatalf("expected failed edit follow-up to be marked as failure, got: %q", got)
	}
	if !strings.Contains(got, "最近一次 Write/Edit 还没成功") {
		t.Fatalf("expected failed edit follow-up to emphasize unfinished mutation, got: %q", got)
	}
	if !strings.Contains(got, "不要声称已完成") {
		t.Fatalf("expected failed edit follow-up to forbid false completion summaries, got: %q", got)
	}
	if !strings.Contains(got, "String to replace not found in file.") {
		t.Fatalf("expected failed edit follow-up to keep the upstream error detail, got: %q", got)
	}
	if !strings.Contains(got, "优先沿用已读或已存在文件做 Edit") {
		t.Fatalf("expected failed edit follow-up to prefer Edit after read/modify flows, got: %q", got)
	}
	if !strings.Contains(got, "首个非空输出字符直接是 `{`") {
		t.Fatalf("expected failed edit follow-up to require direct JSON tool call, got: %q", got)
	}
	if strings.Contains(got, "优先直接向用户总结已完成的修改") {
		t.Fatalf("did not expect success-only continuation guidance after failed edit, got: %q", got)
	}
}

func TestPrepareRequest_DropsSupersededEditFailureAfterLaterReadRetry(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read_1",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read_1",
							Content:   "1→first snapshot\n2→return a + b\n",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_edit_1",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit_1",
							Content:   "<tool_use_error>String to replace not found in file.\nString: old attempt</tool_use_error>",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read_2",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read_2",
							Content:   "1→second snapshot\n2→return add_scientific(a, b)\n",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_edit_2",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit_2",
							Content:   "<tool_use_error>String to replace not found in file.\nString: second attempt</tool_use_error>",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "first snapshot") {
		t.Fatalf("expected superseded read result to be trimmed, got: %q", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "second snapshot") {
		t.Fatalf("expected latest read result to remain, got: %q", got)
	}
	if got := boltReq.Messages[2].Content; strings.Contains(got, "old attempt") {
		t.Fatalf("expected stale edit failure to be trimmed after later read retry, got: %q", got)
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "second attempt") {
		t.Fatalf("expected latest edit failure to remain, got: %q", got)
	}
}

func TestPrepareRequest_DropsEarlierRepeatedEditFailureForSameFile(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_edit_1",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit_1",
							Content:   "<tool_use_error>String to replace not found in file.\nString: stale-1</tool_use_error>",
						},
					},
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_edit_2",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit_2",
							Content:   "<tool_use_error>String to replace not found in file.\nString: stale-2</tool_use_error>",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; strings.Contains(got, "stale-1") {
		t.Fatalf("expected earlier repeated edit failure to be trimmed, got: %q", got)
	}
	if got := boltReq.Messages[1].Content; !strings.Contains(got, "stale-2") {
		t.Fatalf("expected latest repeated edit failure to remain, got: %q", got)
	}
}

func TestBuildBoltToolUsagePrompt_IncludesMutationFailureRecoveryRule(t *testing.T) {
	got := strings.Join(buildBoltToolUsagePrompt([]string{"Read", "Write", "Edit", "Bash"}, []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
	}), "\n")
	if !strings.Contains(got, "若最近一轮 Write/Edit 明确报错") {
		t.Fatalf("expected mutation failure recovery rule in tool prompt, got: %q", got)
	}
	if !strings.Contains(got, "不要沿用更早的成功 Write/Edit 来声称已经更新完成") {
		t.Fatalf("expected tool prompt to override stale success after mutation failure, got: %q", got)
	}
	if !strings.Contains(got, "优先使用 Edit 做最小修改") {
		t.Fatalf("expected tool prompt to prefer Edit for existing files, got: %q", got)
	}
	if !strings.Contains(got, "第一个非空输出字符应当直接是 `{`") {
		t.Fatalf("expected tool prompt to require direct JSON tool calls, got: %q", got)
	}
	if !strings.Contains(got, "不要先对 `.` 做宽泛 Glob") {
		t.Fatalf("expected tool prompt to suppress broad first-turn glob when path is already clear, got: %q", got)
	}
	if !strings.Contains(got, "不要重新从 Glob 开始") {
		t.Fatalf("expected tool prompt to keep following the same file across turns, got: %q", got)
	}
	if !strings.Contains(got, "不要只加显示开关、提示文案或空包装函数") {
		t.Fatalf("expected tool prompt to require substantive feature implementation, got: %q", got)
	}
}

func TestBuildBoltToolUsagePrompt_NonCodingRequestStaysNeutral(t *testing.T) {
	got := strings.Join(buildBoltToolUsagePrompt([]string{"Read", "Write", "Edit"}, []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "现在上海的天气怎么样"}},
	}), "\n")
	if !strings.Contains(got, "若当前问题不需要工具，直接正常回答") {
		t.Fatalf("expected neutral direct-answer guidance, got: %q", got)
	}
	if strings.Contains(got, "优先使用 Edit 做最小修改") {
		t.Fatalf("expected non-coding request to avoid forced mutation guidance, got: %q", got)
	}
	if strings.Contains(got, "默认直接在项目根目录创建 `calculator.py`") {
		t.Fatalf("expected non-coding request to avoid code-task bootstrap guidance, got: %q", got)
	}
}

func TestPrepareRequest_ReadFollowupContinuationPrefersEditAndNoPreamble(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write",
							Content:   "File created successfully at: calculator.py",
						},
					},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   "1→def add(a, b):\n2→    return a + b\n",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) < 1 {
		t.Fatalf("messages len=%d want at least 1", len(boltReq.Messages))
	}
	got := boltReq.Messages[len(boltReq.Messages)-1].Content
	if !strings.Contains(got, "基于这些结果继续回答") {
		t.Fatalf("expected read follow-up continuation to stay neutral, got: %q", got)
	}
	if !strings.Contains(got, "首个非空输出字符直接是 `{`") {
		t.Fatalf("expected read follow-up continuation to require direct JSON, got: %q", got)
	}
	if strings.Contains(got, "优先沿用已读或已存在文件做 Edit") {
		t.Fatalf("expected read follow-up continuation to avoid forced Edit bias, got: %q", got)
	}
	if strings.Contains(got, "我来重写") {
		t.Fatalf("did not expect explanatory preamble in serialized continuation, got: %q", got)
	}
}

func TestPrepareRequest_LeavesProjectPromptEmptyToAvoidDuplicatingGlobalPrompt(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "d:\\Code\\Orchids-2api",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
		},
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Edit"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if strings.TrimSpace(boltReq.GlobalSystemPrompt) == "" {
		t.Fatal("expected global system prompt to remain populated")
	}
	if strings.TrimSpace(boltReq.ProjectPrompt) != "" {
		t.Fatalf("expected project prompt to stay empty to avoid duplicating global prompt, got: %q", boltReq.ProjectPrompt)
	}
}

func TestPrepareRequest_DropsSupersededSuccessfulMutationAfterLaterRead(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_write",
						Name:  "Write",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_write",
						Content:   "File created successfully at: calculator.py",
					}},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   "1→def add(a, b):\n2→    return a + b\n",
					}},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[1].Content; got != "帮我添加科学计数法" {
		t.Fatalf("second message content=%q want latest standalone task", got)
	}
	if got := boltReq.Messages[2].Content; strings.Contains(got, "File created successfully at: calculator.py") {
		t.Fatalf("expected stale successful write result to be trimmed after later read, got: %q", got)
	}
	if got := boltReq.Messages[2].Content; !strings.Contains(got, "def add(a, b)") {
		t.Fatalf("expected latest read result to remain, got: %q", got)
	}
}

func TestPrepareRequest_FollowupMutationAfterSuccessfulCreateGetsGuard(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_write",
						Name:  "Write",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_write",
						Content:   "File created successfully at: calculator.py",
					}},
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	got := boltReq.Messages[2].Content
	if !strings.Contains(got, "帮我添加科学计数法") {
		t.Fatalf("expected latest task to remain, got: %q", got)
	}
	if !strings.Contains(got, "这是新的修改请求") {
		t.Fatalf("expected follow-up mutation task to include fresh-modification guard, got: %q", got)
	}
	if !strings.Contains(got, "`calculator.py`") {
		t.Fatalf("expected guard to mention existing file path, got: %q", got)
	}
	if !strings.Contains(got, "不要直接总结") {
		t.Fatalf("expected guard to forbid false completion summaries, got: %q", got)
	}
}

func TestBuildBoltRetryRequests_UseNeutralCompressedGuidance(t *testing.T) {
	ctx := boltFalseCompletionRetryContext{
		ReadPath: "calculator.py",
		LastTask: "帮我添加科学计数法",
	}

	retry := buildBoltRepeatedReadRetryRequest(upstream.UpstreamRequest{}, ctx)
	if len(retry.Messages) != 1 {
		t.Fatalf("messages len=%d want 1", len(retry.Messages))
	}
	got := retry.Messages[0].Content.GetText()
	if strings.Contains(got, "继续这个明确任务") {
		t.Fatalf("expected retry to avoid task lead injection, got: %q", got)
	}
	if !strings.Contains(got, "RETRY: 刚读过 `calculator.py`") {
		t.Fatalf("expected retry to use short repeated-read code, got: %q", got)
	}
	if !strings.Contains(got, "禁总结，先继续工具修改") {
		t.Fatalf("expected retry to use compressed completion guard, got: %q", got)
	}
	if len([]rune(got)) > 140 {
		t.Fatalf("expected compressed retry message to stay short, got len=%d content=%q", len([]rune(got)), got)
	}
}

func TestShouldRetryBoltFalseCompletion_ForStalePresenceSummary(t *testing.T) {
	ctx := boltFalseCompletionRetryContext{
		HasRecentReadOnlyFollow: true,
		LastTask:                "帮我添加科学计算法",
	}
	converter := &outboundConverter{}
	converter.finalText.WriteString("科学计算功能已在上一轮成功写入 `calculator.py`，无需重复修改。")
	if !shouldRetryBoltFalseCompletion(ctx, converter) {
		t.Fatal("expected stale file-presence completion summary to trigger retry")
	}
}

func TestShouldRetryBoltInvalidPathToolCall_ForSandboxRead(t *testing.T) {
	ctx := boltFalseCompletionRetryContext{
		ReadPath: "calculator.py",
		LastTask: "帮我添加科学计算法",
	}
	converter := &outboundConverter{
		emittedToolUse: true,
		firstToolName:  "Read",
		firstToolPath:  "/tmp/cc-agent/sb1-demo/project/calculator.py",
	}
	if !shouldRetryBoltInvalidPathToolCall(ctx, converter) {
		t.Fatal("expected sandbox read path to trigger invalid-path retry")
	}
}

func TestBuildBoltInvalidPathRetryRequest_UsesNeutralRelativePathHint(t *testing.T) {
	ctx := boltFalseCompletionRetryContext{
		ReadPath: "calculator.py",
		LastTask: "帮我添加科学计算法",
	}
	converter := &outboundConverter{
		firstToolPath: "/tmp/cc-agent/sb1-demo/project/calculator.py",
	}
	retry := buildBoltInvalidPathRetryRequest(upstream.UpstreamRequest{}, ctx, converter)
	if len(retry.Messages) != 1 {
		t.Fatalf("messages len=%d want 1", len(retry.Messages))
	}
	got := retry.Messages[0].Content.GetText()
	if strings.Contains(got, "继续这个明确任务") {
		t.Fatalf("expected retry to avoid task lead injection, got: %q", got)
	}
	if !strings.Contains(got, "无效沙箱路径") || !strings.Contains(got, "/tmp/cc-agent/sb1-demo/project/calculator.py") {
		t.Fatalf("expected retry to mention invalid sandbox path, got: %q", got)
	}
	if !strings.Contains(got, "优先处理 `calculator.py`") {
		t.Fatalf("expected retry to steer back to relative project path, got: %q", got)
	}
}

func TestPrepareRequest_DropsStaleMissingWorkspaceHistory(t *testing.T) {
	workdir := t.TempDir()
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: workdir,
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_write",
						Name:  "Write",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_write",
						Content:   "The file calculator.py has been updated successfully.",
					}},
				},
			},
			{
				Role:    "assistant",
				Content: prompt.MessageContent{Text: "计算器已经完整实现，文件 `calculator.py` 可直接运行。"},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role:    "user",
				Content: prompt.MessageContent{Text: "上一轮你在没有任何新的成功 Write/Edit 工具结果的情况下直接声称已经完成，这是错误的。你上一轮只有 Read 结果，没有任何成功的 Write/Edit，因此不能根据读取结果声称已经添加完功能。请直接基于刚读到的`calculator.py`内容继续调用 Edit/Write 完成修改；不要把“文件存在”、“Glob 找到了文件”、或更早创建成功当成当前任务已经完成。除非先出现新的成功 Write/Edit 工具结果，否则不要输出“已更新”“已完成”“可以运行”等完成总结。如果决定调用工具，本回合第一个非空输出字符必须直接是 `{`。"},
			},
			{
				Role:    "user",
				Content: prompt.MessageContent{Text: "你刚刚已经成功 Read 过`calculator.py`，但又再次请求 Read 同一路径，这属于无效重复读取。当前任务是继续修改代码，不是继续确认文件是否存在。除非上一份 Read 结果因为截断而确实缺少你马上要修改的那一小段必要文本，否则不要再次 Read 同一路径。请直接基于已经读到的`calculator.py`内容继续调用 Edit/Write 完成修改；如果文件已存在，优先 Edit。在出现新的成功 Write/Edit 工具结果之前，不要输出“已完成”“已更新”“可以运行”等总结。如果决定调用工具，本回合第一个非空输出字符必须直接是 `{`。"},
			},
		},
		Tools: []interface{}{
			map[string]interface{}{"name": "Read"},
			map[string]interface{}{"name": "Write"},
			map[string]interface{}{"name": "Edit"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "帮我用python写一个计算器" {
		t.Fatalf("first message content=%q want original create intent", got)
	}
	if got := boltReq.Messages[1].Content; got != "帮我添加科学计数法" {
		t.Fatalf("second message content=%q want latest edit intent", got)
	}
	if got := strings.Join([]string{boltReq.Messages[0].Content, boltReq.Messages[1].Content}, "\n"); strings.Contains(got, "已更新") || strings.Contains(got, "可直接运行") || strings.Contains(got, "你刚刚已经成功 Read 过`calculator.py`") {
		t.Fatalf("expected stale deleted-file history to be trimmed from remaining messages, got: %q", got)
	}
}

func TestFormatBoltToolResultContinuation_CompressesGeneralFollowupPrompt(t *testing.T) {
	got := formatBoltToolResultContinuation(false, false, false, nil)
	if !strings.Contains(got, "基于这些结果继续回答") {
		t.Fatalf("expected neutral continuation guidance, got: %q", got)
	}
	if !strings.Contains(got, "下面是上一轮工具结果") {
		t.Fatalf("expected continuation to stay neutral about prior tool results, got: %q", got)
	}
	if strings.Contains(got, "优先沿用已读或已存在文件做 Edit") {
		t.Fatalf("expected general continuation to avoid forced Edit bias, got: %q", got)
	}
	if !strings.Contains(got, "首个非空输出字符直接是 `{`") {
		t.Fatalf("expected continuation to require direct JSON tool calls, got: %q", got)
	}
	if strings.Contains(got, "不要重新打招呼，也不要把它当成新的空白任务") {
		t.Fatalf("expected compressed continuation without old verbose wording, got: %q", got)
	}
	if len([]rune(got)) > 260 {
		t.Fatalf("expected compressed continuation to stay short, got len=%d content=%q", len([]rune(got)), got)
	}
}

func TestFormatBoltToolResultContinuation_IncludesPriorToolContext(t *testing.T) {
	got := formatBoltToolResultContinuation(false, false, false, &boltSerializedToolResult{
		ToolName: "Read",
		ToolPath: "/usr/lib/node_modules/openclaw/skills/weather/SKILL.md",
	})
	if !strings.Contains(got, "上一轮工具: Read(/usr/lib/node_modules/openclaw/skills/weather/SKILL.md)") {
		t.Fatalf("expected prior tool context in continuation, got: %q", got)
	}
}

func TestFormatBoltToolResultContinuation_SuccessPromptAvoidsFeatureHallucination(t *testing.T) {
	got := formatBoltToolResultContinuation(false, true, false, nil)
	if !strings.Contains(got, "只做最小确认") {
		t.Fatalf("expected success continuation to enforce minimal confirmation, got: %q", got)
	}
	if !strings.Contains(got, "不要补充文件内容细节") {
		t.Fatalf("expected success continuation to forbid hallucinated file details, got: %q", got)
	}
	if !strings.Contains(got, "已创建/更新相应文件") {
		t.Fatalf("expected success continuation to stay at coarse confirmation level, got: %q", got)
	}
	if strings.Contains(got, "继续完成用户刚才明确提出的任务") {
		t.Fatalf("expected success continuation to avoid task-continuation framing, got: %q", got)
	}
	if len([]rune(got)) > 70 {
		t.Fatalf("expected compressed success continuation to stay short, got len=%d content=%q", len([]rune(got)), got)
	}
}

func TestPrepareRequest_TruncatesLongReadResults(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我分析这个文件"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "README.md"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   strings.Repeat("1234567890", 800),
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	got := boltReq.Messages[1].Content
	if !strings.Contains(got, "truncated read output") && !strings.Contains(got, "truncated active file excerpt") {
		t.Fatalf("expected long read result to be truncated, got: %q", got)
	}
	if len([]rune(got)) >= len([]rune("Tool result:\n"+strings.Repeat("1234567890", 800))) {
		t.Fatalf("expected truncated payload to be shorter, got len=%d", len([]rune(got)))
	}
	if strings.Contains(got, "call Read again") {
		t.Fatalf("expected generic read truncation hint to avoid encouraging immediate reread, got: %q", got)
	}
}

func TestPrepareRequest_KeepsLargerReadWindowForFocusedFile(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我给 calculator.py 添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_read",
							Content:   strings.Repeat("1234567890", 180),
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	got := boltReq.Messages[1].Content
	if strings.Contains(got, "truncated read output") {
		t.Fatalf("expected focused file read result to keep a larger window, got: %q", got)
	}
	if !strings.Contains(got, strings.Repeat("1234567890", 120)) {
		t.Fatalf("expected focused file read result to retain long content, got: %q", got)
	}
}

func TestPrepareRequest_TruncatedFocusedReadResultDoesNotEncourageImmediateReread(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我给 calculator.py 添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:  "tool_use",
						ID:    "tool_read",
						Name:  "Read",
						Input: map[string]interface{}{"file_path": "calculator.py"},
					}},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{{
						Type:      "tool_result",
						ToolUseID: "tool_read",
						Content:   strings.Repeat("1234567890", 700),
					}},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2", len(boltReq.Messages))
	}
	got := boltReq.Messages[1].Content
	if !strings.Contains(got, "truncated active file excerpt") {
		t.Fatalf("expected focused file read result to still truncate when very long, got: %q", got)
	}
	if strings.Contains(got, "call Read again") {
		t.Fatalf("expected focused read truncation hint to avoid encouraging immediate reread, got: %q", got)
	}
	if !strings.Contains(got, "prefer editing from the visible excerpt") {
		t.Fatalf("expected focused read truncation hint to prefer editing visible excerpt, got: %q", got)
	}
}

func TestPrepareRequest_DropsInvalidPathResultsAndSupersededEditErrors(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_abs_read",
							Name:  "Read",
							Input: map[string]interface{}{"file_path": "C:\\Users\\zhangdailin\\Desktop\\111\\calculator.py"},
						},
						{
							Type:  "tool_use",
							ID:    "tool_edit",
							Name:  "Edit",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
						{
							Type:  "tool_use",
							ID:    "tool_abs_write",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "C:\\Users\\zhangdailin\\Desktop\\111\\calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_abs_read",
							Content:   "1→import math",
						},
						{
							Type:      "tool_result",
							ToolUseID: "tool_edit",
							Content:   "<tool_use_error>String to replace not found in file.</tool_use_error>",
						},
						{
							Type:      "tool_result",
							ToolUseID: "tool_abs_write",
							Content:   "The file C:\\Users\\zhangdailin\\Desktop\\111\\calculator.py has been updated successfully.",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 1 {
		t.Fatalf("messages len=%d want 1, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "添加科学计数法" {
		t.Fatalf("first message content=%q want original user prompt only", got)
	}
}

func TestPrepareRequest_RelativizesWorkspaceAbsoluteGlobResults(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:   "claude-opus-4-6",
		Workdir: "C:\\Users\\zhangdailin\\Desktop\\1212",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我添加科学计数法"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_glob",
							Name:  "Glob",
							Input: map[string]interface{}{"path": ".", "pattern": "**/*"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_glob",
							Content:   "C:\\Users\\zhangdailin\\Desktop\\1212\\calculator.py",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 2 {
		t.Fatalf("messages len=%d want 2, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	got := boltReq.Messages[1].Content
	if strings.Contains(got, "C:\\Users\\zhangdailin\\Desktop\\1212\\calculator.py") {
		t.Fatalf("expected workspace absolute path to be relativized, got: %q", got)
	}
	if !strings.Contains(got, "Tool result:\ncalculator.py") {
		t.Fatalf("expected relativized workspace path to remain in history, got: %q", got)
	}
}

func TestIsBoltToolResultError_RecognizesHookDeniedWrite(t *testing.T) {
	block := prompt.ContentBlock{
		Type: "tool_result",
	}
	if !isBoltToolResultError(block, "Hook PreToolUse:Write denied this tool") {
		t.Fatal("expected hook-denied write result to be treated as an error")
	}
}

func TestPrepareRequest_DropsSupersededHookDeniedWriteResults(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write_rel",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "calculator.py"},
						},
						{
							Type:  "tool_use",
							ID:    "tool_write_tmp",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "/tmp/cc-agent/sb1-demo/project/calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write_rel",
							Content:   "Hook PreToolUse:Write denied this tool",
						},
						{
							Type:      "tool_result",
							ToolUseID: "tool_write_tmp",
							Content:   "File created successfully at: /tmp/cc-agent/sb1-demo/project/calculator.py",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 1 {
		t.Fatalf("messages len=%d want 1, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "帮我用python写一个计算器" {
		t.Fatalf("first message content=%q want original user prompt only", got)
	}
}

func TestPrepareRequest_DropsSandboxWriteResultsFromHistory(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "继续"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:  "tool_use",
							ID:    "tool_write_tmp",
							Name:  "Write",
							Input: map[string]interface{}{"file_path": "/tmp/cc-agent/sb1-demo/project/calculator.py"},
						},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{
							Type:      "tool_result",
							ToolUseID: "tool_write_tmp",
							Content:   "File created successfully at: /tmp/cc-agent/sb1-demo/project/calculator.py",
						},
					},
				},
			},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 1 {
		t.Fatalf("messages len=%d want 1, messages=%#v", len(boltReq.Messages), boltReq.Messages)
	}
	if got := boltReq.Messages[0].Content; got != "继续" {
		t.Fatalf("first message content=%q want original user prompt only", got)
	}
}

func TestSupportedBoltToolNames_DoesNotDefaultWhenRequestOmitsTools(t *testing.T) {
	if got := supportedBoltToolNames(nil); got != nil {
		t.Fatalf("supportedBoltToolNames(nil) = %#v want nil", got)
	}
}

func TestSupportedBoltToolNames_DoesNotInventCoreToolsWhenOnlyUnsupportedToolsExist(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "TodoWrite"},
	}

	if got := supportedBoltToolNames(tools); got != nil {
		t.Fatalf("supportedBoltToolNames(unsupported) = %#v want nil", got)
	}
}

func TestSupportedBoltToolNames_AllowsSkill(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "Skill"},
		map[string]interface{}{"name": "Read"},
	}

	got := supportedBoltToolNames(tools)
	want := []string{"Read", "Skill"}
	if len(got) != len(want) {
		t.Fatalf("supportedBoltToolNames(skill) len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedBoltToolNames(skill)[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestSupportedBoltToolNames_MapsAgentToTask(t *testing.T) {
	tools := []interface{}{
		map[string]interface{}{"name": "Read"},
		map[string]interface{}{"name": "Agent"},
	}

	got := supportedBoltToolNames(tools)
	want := []string{"Read", "Task"}
	if len(got) != len(want) {
		t.Fatalf("supportedBoltToolNames(agent) len=%d want=%d (%#v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("supportedBoltToolNames(agent)[%d]=%q want %q (%#v)", i, got[i], want[i], got)
		}
	}
}

func TestPrepareRequest_AdvertisesTaskWhenClientDeclaresAgent(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "explore this repo"}}},
		Tools: []interface{}{
			map[string]interface{}{"name": "Agent"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Task(description, prompt, subagent_type?)") {
		t.Fatalf("global prompt missing Task hint: %q", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "客户端声明的是 `Agent`") {
		t.Fatalf("global prompt missing Agent/Task relay guidance: %q", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_AdvertisesSkillWhenClientDeclaresSkill(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "今天扬州天气怎么样"}}},
		Tools: []interface{}{
			map[string]interface{}{"name": "Skill"},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !strings.Contains(boltReq.GlobalSystemPrompt, "Skill(skill, args)") {
		t.Fatalf("global prompt missing Skill hint: %q", boltReq.GlobalSystemPrompt)
	}
	if !strings.Contains(boltReq.GlobalSystemPrompt, "上游 Bolt 可能直接返回 `Skill(skill, args)`") {
		t.Fatalf("global prompt missing Skill relay guidance: %q", boltReq.GlobalSystemPrompt)
	}
}

func TestPrepareRequest_PreservesFreshTaskFirstPromptSignal(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:         "claude-sonnet-4-6",
		IsFirstPrompt: true,
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if !boltReq.IsFirstPrompt {
		t.Fatal("expected prepareRequest to keep upstream first-prompt signal")
	}
}

func TestPrepareRequest_SkipsToolRoleMessages(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "hello"}},
			{
				Role: "tool",
				Content: prompt.MessageContent{
					Text: "Model tried to call unavailable tool 'WebSearch'. Available tools: builtin_web_search.",
				},
			},
			{Role: "user", Content: prompt.MessageContent{Text: "现在上海的天气怎么样"}},
		},
	}

	boltReq, _ := prepareRequest(req, "sb1-demo")
	if len(boltReq.Messages) != 3 {
		t.Fatalf("messages len=%d want 3", len(boltReq.Messages))
	}
	for _, msg := range boltReq.Messages {
		if msg.Role == "tool" {
			t.Fatalf("unexpected tool role message in bolt request: %#v", msg)
		}
		if strings.Contains(msg.Content, "unavailable tool") {
			t.Fatalf("unexpected tool error content in bolt request: %#v", msg)
		}
	}
}

func TestFetchRootData_UsesSessionCookie(t *testing.T) {
	prevRootURL := boltRootDataURL
	t.Cleanup(func() { boltRootDataURL = prevRootURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "_data=root" {
			t.Fatalf("unexpected query: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"root-token","user":{"id":"user_1","email":"bolt@example.com","totalBoltTokenPurchases":1000000}}`)
	}))
	defer srv.Close()
	boltRootDataURL = srv.URL + "?_data=root"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRootData(context.Background())
	if err != nil {
		t.Fatalf("FetchRootData() error = %v", err)
	}
	if data.Token != "root-token" || data.User == nil || data.User.ID != "user_1" {
		t.Fatalf("unexpected root data: %+v", data)
	}
	if data.User.TotalBoltTokenPurchases != 1_000_000 {
		t.Fatalf("totalBoltTokenPurchases=%v want 1000000", data.User.TotalBoltTokenPurchases)
	}
}

func TestFetchRateLimits_UsesSessionCookieAndUserPath(t *testing.T) {
	prevRateURL := boltRateLimitsURL
	prevTeamsRateURL := boltTeamsRateLimitsURL
	t.Cleanup(func() {
		boltRateLimitsURL = prevRateURL
		boltTeamsRateLimitsURL = prevTeamsRateURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rate-limits/user" {
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
			t.Fatalf("cookie=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"maxPerMonth":10000000,"regularTokens":{"available":10000000,"used":255061},"purchased":{"available":1000000,"used":0},"rewardTokens":{"available":0,"used":0},"specialTokens":{"available":0,"used":0},"referralTokens":{"free":{"available":0,"used":0},"paid":{"available":0,"used":0}},"totalThisMonth":255061,"totalToday":255061}`)
	}))
	defer srv.Close()

	boltRateLimitsURL = srv.URL + "/api/rate-limits/user"
	boltTeamsRateLimitsURL = srv.URL + "/api/rate-limits/teams"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRateLimits(context.Background(), 0)
	if err != nil {
		t.Fatalf("FetchRateLimits() error = %v", err)
	}
	if data.MaxPerMonth != 10_000_000 {
		t.Fatalf("maxPerMonth=%v want 10000000", data.MaxPerMonth)
	}
	if data.RegularTokens == nil || data.RegularTokens.Used != 255061 {
		t.Fatalf("regularTokens=%+v", data.RegularTokens)
	}
}

func TestFetchRateLimits_UsesTeamPathWhenOrganizationSelected(t *testing.T) {
	prevRateURL := boltRateLimitsURL
	prevTeamsRateURL := boltTeamsRateLimitsURL
	t.Cleanup(func() {
		boltRateLimitsURL = prevRateURL
		boltTeamsRateLimitsURL = prevTeamsRateURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/rate-limits/teams/42" {
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"maxPerMonth":26000000,"regularTokens":{"available":26000000,"used":0},"purchased":{"available":0,"used":0},"rewardTokens":{"available":0,"used":0},"specialTokens":{"available":0,"used":0},"referralTokens":{"free":{"available":0,"used":0},"paid":{"available":0,"used":0}},"totalThisMonth":0,"totalToday":0}`)
	}))
	defer srv.Close()

	boltRateLimitsURL = srv.URL + "/api/rate-limits/user"
	boltTeamsRateLimitsURL = srv.URL + "/api/rate-limits/teams"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	data, err := client.FetchRateLimits(context.Background(), 42)
	if err != nil {
		t.Fatalf("FetchRateLimits() error = %v", err)
	}
	if data.MaxPerMonth != 26_000_000 {
		t.Fatalf("maxPerMonth=%v want 26000000", data.MaxPerMonth)
	}
}

func TestCreateEmptyProject_UsesBearerTokenAndReturnsSlug(t *testing.T) {
	prevProjectsURL := boltProjectsCreateURL
	t.Cleanup(func() { boltProjectsCreateURL = prevProjectsURL })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%q want POST", r.Method)
		}
		if r.URL.Path != "/api/projects/sb1/fork" {
			t.Fatalf("path=%q want /api/projects/sb1/fork", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("authorization=%q want bearer token", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"appFiles":{}`) {
			t.Fatalf("request body missing empty appFiles: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":64985691,"slug":"sb1-demo-new"}`)
	}))
	defer srv.Close()

	boltProjectsCreateURL = srv.URL + "/api/projects/sb1/fork"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		Token:         "api-token",
		ProjectID:     "sb1-demo",
	}, nil)

	projectID, err := client.CreateEmptyProject(context.Background())
	if err != nil {
		t.Fatalf("CreateEmptyProject() error = %v", err)
	}
	if projectID != "sb1-demo-new" {
		t.Fatalf("projectID=%q want sb1-demo-new", projectID)
	}
}

func TestCreateEmptyProject_FetchesRootTokenWhenMissing(t *testing.T) {
	prevProjectsURL := boltProjectsCreateURL
	prevRootURL := boltRootDataURL
	t.Cleanup(func() {
		boltProjectsCreateURL = prevProjectsURL
		boltRootDataURL = prevRootURL
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root":
			if got := r.Header.Get("Cookie"); !strings.Contains(got, "__session=session-token") {
				t.Fatalf("cookie=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"root-token"}`)
		case "/api/projects/sb1/fork":
			if got := r.Header.Get("Authorization"); got != "Bearer root-token" {
				t.Fatalf("authorization=%q want bearer root-token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"slug":"sb1-created-from-root"}`)
		default:
			t.Fatalf("unexpected path: %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	boltRootDataURL = srv.URL + "/root"
	boltProjectsCreateURL = srv.URL + "/api/projects/sb1/fork"

	client := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-token",
		ProjectID:     "sb1-demo",
	}, nil)

	projectID, err := client.CreateEmptyProject(context.Background())
	if err != nil {
		t.Fatalf("CreateEmptyProject() error = %v", err)
	}
	if projectID != "sb1-created-from-root" {
		t.Fatalf("projectID=%q want sb1-created-from-root", projectID)
	}
}

func TestNewFromAccount_ReusesSharedHTTPClient(t *testing.T) {
	cfg := &config.Config{
		RequestTimeout: 30,
		ProxyHTTP:      "http://proxy.local:3128",
		ProxyUser:      "user",
		ProxyPass:      "pass",
	}

	clientA := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-a",
		ProjectID:     "sb1-a",
	}, cfg)
	clientB := NewFromAccount(&store.Account{
		AccountType:   "bolt",
		SessionCookie: "session-b",
		ProjectID:     "sb1-b",
	}, cfg)

	if clientA.httpClient != clientB.httpClient {
		t.Fatal("expected bolt clients with same transport config to reuse shared http client")
	}
	if !clientA.sharedHTTPClient || !clientB.sharedHTTPClient {
		t.Fatal("expected sharedHTTPClient flag to be set")
	}
}
