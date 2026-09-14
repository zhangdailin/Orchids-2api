package puter

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestSplitMultiToolCallsSplitsPairedTurns(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "I will read two files", ToolCalls: []ToolCall{
			{ID: "call_a", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"a"}`}},
			{ID: "call_b", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"b"}`}},
		}},
		{Role: "tool", ToolCallID: "call_a", Content: "contents of a"},
		{Role: "tool", ToolCallID: "call_b", Content: "contents of b"},
		{Role: "user", Content: "summarize"},
	}

	got := splitMultiToolCalls(in)
	if len(got) != 5 {
		t.Fatalf("len=%d want 5: %#v", len(got), got)
	}
	if got[0].Role != "assistant" || len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "call_a" || got[0].Content != "I will read two files" {
		t.Fatalf("got[0]=%#v", got[0])
	}
	if got[1].Role != "tool" || got[1].ToolCallID != "call_a" {
		t.Fatalf("got[1]=%#v", got[1])
	}
	if got[2].Role != "assistant" || len(got[2].ToolCalls) != 1 || got[2].ToolCalls[0].ID != "call_b" || got[2].Content != "" {
		t.Fatalf("got[2]=%#v", got[2])
	}
	if got[3].Role != "tool" || got[3].ToolCallID != "call_b" {
		t.Fatalf("got[3]=%#v", got[3])
	}
	if got[4].Role != "user" || got[4].Content != "summarize" {
		t.Fatalf("got[4]=%#v", got[4])
	}
}

func TestSplitMultiToolCallsThreeCalls(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Function: ToolCallFunction{Name: "Grep", Arguments: `{}`}},
			{ID: "c2", Function: ToolCallFunction{Name: "Grep", Arguments: `{}`}},
			{ID: "c3", Function: ToolCallFunction{Name: "Grep", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "tool", ToolCallID: "c3", Content: "r3"},
	}
	got := splitMultiToolCalls(in)
	if len(got) != 6 {
		t.Fatalf("len=%d want 6: %#v", len(got), got)
	}
	for k, id := range []string{"c1", "c2", "c3"} {
		if got[k*2].Role != "assistant" || got[k*2].ToolCalls[0].ID != id {
			t.Fatalf("part[%d]=%#v", k*2, got[k*2])
		}
		if got[k*2+1].Role != "tool" || got[k*2+1].ToolCallID != id {
			t.Fatalf("part[%d]=%#v", k*2+1, got[k*2+1])
		}
	}
}

func TestSplitMultiToolCallsLeavesIncompleteTurnsUntouched(t *testing.T) {
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_a", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
			{ID: "call_b", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
		}},
		// 只有 call_a 的回应,缺少 call_b。
		{Role: "tool", ToolCallID: "call_a", Content: "r1"},
		{Role: "user", Content: "hello"},
	}
	got := splitMultiToolCalls(in)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (unchanged): %#v", len(got), got)
	}
	if len(got[0].ToolCalls) != 2 || got[0].Role != "assistant" {
		t.Fatalf("got[0]=%#v want original multi-call assistant", got[0])
	}
}

func TestSplitMultiToolCallsIgnoresNonToolScenarios(t *testing.T) {
	in := []Message{
		{Role: "system", Content: "sys"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "one", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "one", Content: "r"},
	}
	got := splitMultiToolCalls(in)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3: %#v", len(got), got)
	}
	// 单个 tool_call 不拆分。
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].ID != "one" {
		t.Fatalf("got[1]=%#v", got[1])
	}
}

// 显式传入 SystemItem;convertMessages 将 system 条目逐字透传为独立 system 消息。
func TestBuildRequestSplitsMultiToolCallsOnlyForDeepseek(t *testing.T) {
	build := func(model string) []Message {
		client := NewFromAccount(nil, nil)
		req, err := client.buildRequest(upstream.UpstreamRequest{
			Model:  model,
			System: []prompt.SystemItem{{Type: "text", Text: "sys"}},
			Messages: []prompt.Message{
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "a", Name: "Read", Input: map[string]interface{}{"file_path": "x"}},
					{Type: "tool_use", ID: "b", Name: "Read", Input: map[string]interface{}{"file_path": "y"}},
				}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "a", Content: "r1"},
					{Type: "tool_result", ToolUseID: "b", Content: "r2"},
				}}},
			},
		}, false)
		if err != nil {
			t.Fatalf("buildRequest(%q) error=%v", model, err)
		}
		return req.Args.Messages
	}

	deepseek := build("deepseek-v4-flash")
	// 1 条 system + 拆分后的 4 条(assistant(a)/tool(a)/assistant(b)/tool(b))
	if len(deepseek) != 5 {
		t.Fatalf("deepseek messages len=%d want 5: %#v", len(deepseek), deepseek)
	}
	if deepseek[0].Role != "system" {
		t.Fatalf("deepseek[0]=%#v want system", deepseek[0])
	}
	if deepseek[1].Role != "assistant" || len(deepseek[1].ToolCalls) != 1 {
		t.Fatalf("deepseek[1]=%#v", deepseek[1])
	}

	claude := build("claude-opus-5")
	// 1 条 system + 未拆分(assistant 带 2 tool_calls + 2 tool)
	if len(claude) != 4 {
		t.Fatalf("claude messages len=%d want 4 (unsplit): %#v", len(claude), claude)
	}
	if len(claude[1].ToolCalls) != 2 {
		t.Fatalf("claude[1]=%#v want unsplit multi-call", claude[1])
	}
}

func TestBuildRequestForwardsToolControls(t *testing.T) {
	parallel := false
	client := NewFromAccount(nil, nil)
	req, err := client.buildRequest(upstream.UpstreamRequest{
		Model: "gpt-5.6-sol",
		Tools: []interface{}{map[string]interface{}{
			"name": "read", "input_schema": map[string]interface{}{"type": "object"},
		}},
		ToolChoice:        map[string]interface{}{"type": "tool", "name": "read"},
		ParallelToolCalls: &parallel,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	choice, _ := req.Args.ToolChoice.(map[string]interface{})
	function, _ := choice["function"].(map[string]interface{})
	if choice["type"] != "function" || function["name"] != "read" {
		t.Fatalf("tool choice=%#v", req.Args.ToolChoice)
	}
	if req.Args.ParallelToolCalls == nil || *req.Args.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls=%v want false", req.Args.ParallelToolCalls)
	}
}

func TestSplitMultiToolCallsReordersToolResultsCorrectly(t *testing.T) {
	// tool 回应顺序与 tool_calls 不一致时,按 tool_calls 顺序成对输出。
	in := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
			{ID: "b", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "b", Content: "rb"},
		{Role: "tool", ToolCallID: "a", Content: "ra"},
	}
	got := splitMultiToolCalls(in)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4: %#v", len(got), got)
	}
	if got[0].ToolCalls[0].ID != "a" || got[1].ToolCallID != "a" {
		t.Fatalf("want a-pair first: %#v %#v", got[0], got[1])
	}
	if got[2].ToolCalls[0].ID != "b" || got[3].ToolCallID != "b" {
		t.Fatalf("want b-pair second: %#v %#v", got[2], got[3])
	}
}

// 该用例保证 TestBuildRequestSplitsMultiToolCallsOnlyForDeepseek 的断言真实有效:
// 使用 buildRequest 语义(不走流式),验证拆分会真实出现在 deepseek 请求中。
func TestSplitMultiToolCallsPreservesArgumentsText(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "lead", ToolCalls: []ToolCall{
			{ID: "a", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"x"}`}},
			{ID: "b", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"y"}`}},
		}},
		{Role: "tool", ToolCallID: "a", Content: "r1"},
		{Role: "tool", ToolCallID: "b", Content: "r2"},
	}
	got := splitMultiToolCalls(in)
	joined := got[0].Content + got[0].ToolCalls[0].Function.Arguments + got[2].ToolCalls[0].Function.Arguments
	if !strings.Contains(joined, "lead") || !strings.Contains(joined, "x") || !strings.Contains(joined, "y") {
		t.Fatalf("content/arguments lost: %q", joined)
	}
}

// 思考模式下,DeepSeek 要求 assistant 消息回传 reasoning_content;否则上游 400。
// convertMessages 应把 Anthropic 风格 `thinking` 块与 OpenAI 顶层 reasoning_content
// 都汇入 puter Message.ReasoningContent(仅 deepseek 服务开启回传)。
func TestConvertMessagesEchoesReasoningForDeepseek(t *testing.T) {
	msgs := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "thinking", Thinking: "let me reason step by step"},
			{Type: "text", Text: "final answer"},
		}}},
		{Role: "user", Content: prompt.MessageContent{Text: "next"}},
	}
	out := convertMessages(msgs, nil, true)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2: %#v", len(out), out)
	}
	if out[0].Role != "assistant" || out[0].Content != "final answer" || out[0].ReasoningContent != "let me reason step by step" {
		t.Fatalf("assistant=%#v want text+reasoning echoed", out[0])
	}
}

func TestConvertMessagesEchoesOpenAIReasoningContentField(t *testing.T) {
	msgs := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Text: "done"}, ReasoningContent: "thought it through"},
	}
	out := convertMessages(msgs, nil, true)
	if len(out) != 1 || out[0].ReasoningContent != "thought it through" {
		t.Fatalf("got %#v want reasoning_content echoed from top-level field", out)
	}
}

func TestConvertMessagesSuppliesFallbackWhenClientDropsDeepSeekReasoning(t *testing.T) {
	msgs := []prompt.Message{{
		Role: "assistant",
		Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "..."},
			{Type: "tool_use", ID: "call-1", Name: "Pwsh", Input: map[string]interface{}{"command": "python --version"}},
		}},
	}}
	out := convertMessages(msgs, nil, true)
	if len(out) != 1 || out[0].ReasoningContent != missingDeepSeekReasoningFallback {
		t.Fatalf("got %#v want non-empty fallback reasoning", out)
	}
	if out[0].ReasoningContent != "\u200b" {
		t.Fatalf("fallback must remain semantically invisible, got %q", out[0].ReasoningContent)
	}
}

// 非 deepseek 服务不回传 reasoning,与旧版行为一致。
func TestConvertMessagesSkipsReasoningForOtherServices(t *testing.T) {
	msgs := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "thinking", Thinking: "secret reasoning"},
			{Type: "text", Text: "answer"},
		}}},
	}
	out := convertMessages(msgs, nil, false)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1: %#v", len(out), out)
	}
	if out[0].ReasoningContent != "" {
		t.Fatalf("non-deepseek must drop reasoning, got %#v", out[0])
	}
	if out[0].Content != "answer" {
		t.Fatalf("content=%q want %q", out[0].Content, "answer")
	}
}

func TestConvertMessagesDropsDanglingAndDuplicateToolResults(t *testing.T) {
	messages := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
			Type: "tool_use", ID: "call-1", Name: "read", Input: map[string]interface{}{},
		}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-1", Content: "ok"},
			{Type: "tool_result", ToolUseID: "call-1", Content: "duplicate"},
			{Type: "tool_result", ToolUseID: "missing", Content: "dangling"},
		}}},
	}
	got := convertMessages(messages, nil, false)
	toolMessages := 0
	for _, message := range got {
		if message.Role == "tool" {
			toolMessages++
			if message.ToolCallID != "call-1" || message.Content != "ok" {
				t.Fatalf("unexpected tool message: %#v", message)
			}
		}
	}
	if toolMessages != 1 {
		t.Fatalf("tool messages=%d want 1: %#v", toolMessages, got)
	}
}

// 拆分多 tool_call 后的每个合成 assistant 都必须携带 reasoning_content，
// 否则 DeepSeek 会把后续分片视为缺少思考内容的新 assistant 轮次。
func TestSplitMultiToolCallsPreservesReasoningOnEveryPart(t *testing.T) {
	in := []Message{
		{Role: "assistant", Content: "lead", ReasoningContent: "reasoning here", ToolCalls: []ToolCall{
			{ID: "a", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
			{ID: "b", Function: ToolCallFunction{Name: "Read", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "a", Content: "r1"},
		{Role: "tool", ToolCallID: "b", Content: "r2"},
	}
	got := splitMultiToolCalls(in)
	if got[0].ReasoningContent != "reasoning here" {
		t.Fatalf("got[0].ReasoningContent=%q want %q", got[0].ReasoningContent, "reasoning here")
	}
	if got[0].Content != "lead" {
		t.Fatalf("got[0].Content=%q want %q", got[0].Content, "lead")
	}
	if got[2].ReasoningContent != "reasoning here" {
		t.Fatalf("second split part reasoning=%q want %q", got[2].ReasoningContent, "reasoning here")
	}
}

func TestMergeAdjacentAssistantMessagesPreservesToolsAndRealReasoning(t *testing.T) {
	in := []Message{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: "first", ReasoningContent: missingDeepSeekReasoningFallback},
		{Role: "assistant", ReasoningContent: "real reasoning", ToolCalls: []ToolCall{{
			ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "grep", Arguments: `{}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Content: "done"},
	}

	got := mergeAdjacentAssistantMessages(in)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3: %#v", len(got), got)
	}
	if got[1].Content != "first" || got[1].ReasoningContent != "real reasoning" || len(got[1].ToolCalls) != 1 {
		t.Fatalf("merged assistant=%#v", got[1])
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call-1" {
		t.Fatalf("tool result pairing changed: %#v", got[2])
	}
}

func TestBuildRequestCoalescesAdjacentDeepSeekAssistants(t *testing.T) {
	client := NewFromAccount(nil, nil)
	req, err := client.buildRequest(upstream.UpstreamRequest{
		Model: "deepseek-v4-flash",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "start"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "working"}},
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
				Type: "tool_use", ID: "call-1", Name: "grep", Input: map[string]interface{}{"pattern": "signup"},
			}}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
				Type: "tool_result", ToolUseID: "call-1", Content: "done",
			}}}},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Args.Messages) != 3 {
		t.Fatalf("messages=%#v want user, merged assistant, tool", req.Args.Messages)
	}
	assistant := req.Args.Messages[1]
	if assistant.Role != "assistant" || assistant.Content != "working" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("merged assistant=%#v", assistant)
	}
	if req.Args.Messages[2].Role != "tool" || req.Args.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("tool result=%#v", req.Args.Messages[2])
	}
}
