package warp

import (
	"slices"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestBuildRequestBytes_UsesOfficialProtoRequest(t *testing.T) {
	taskContext, err := proto.Marshal(warpapi.Request_TaskContext_builder{
		Tasks: []*warpapi.Task{warpapi.Task_builder{Id: stringPtr("task-1")}.Build()},
	}.Build())
	if err != nil {
		t.Fatalf("marshal task context: %v", err)
	}
	req := upstream.UpstreamRequest{
		Model:           "claude-4-5-sonnet",
		Workdir:         "/repo",
		ChatSessionID:   "warp_conv_1",
		WarpTaskContext: taskContext,
		Messages: []prompt.Message{
			{
				Role: "user",
				Content: prompt.MessageContent{
					Text: "check the project layout",
				},
			},
			{
				Role: "assistant",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "text", Text: "I will inspect the repository."},
						{Type: "tool_use", ID: "call_1", Name: "Glob", Input: map[string]interface{}{"pattern": "**/*"}},
					},
				},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{
					Blocks: []prompt.ContentBlock{
						{Type: "tool_result", ToolUseID: "call_1", Content: "./README.md\n./main.go"},
					},
				},
			},
		},
	}

	query, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("expected protobuf payload")
	}
	if strings.Contains(query, "<|system_prompt|>") || strings.Contains(query, "<|conversation|>") {
		t.Fatalf("query should not contain legacy template markers: %q", query)
	}
	if query != "" {
		t.Fatalf("tool-only continuation query=%q want empty", query)
	}

	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request proto: %v", err)
	}
	if decoded.GetInput().WhichType() != warpapi.Request_Input_UserInputs_case {
		t.Fatalf("input type=%v want user_inputs", decoded.GetInput().WhichType())
	}
	if tasks := decoded.GetTaskContext().GetTasks(); len(tasks) != 1 || tasks[0].GetId() != "task-1" {
		t.Fatalf("task context=%#v want task-1", tasks)
	}
	inputs := decoded.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 {
		t.Fatalf("user inputs=%d want 1", len(inputs))
	}
	toolResult := inputs[0].GetToolCallResult()
	if toolResult == nil {
		t.Fatal("user input missing typed tool result")
	}
	if got := toolResult.GetToolCallId(); got != "call_1" {
		t.Fatalf("tool_call_id=%q want call_1", got)
	}
	mcpResult := toolResult.GetCallMcpTool()
	if mcpResult == nil || mcpResult.GetSuccess() == nil || len(mcpResult.GetSuccess().GetResults()) != 1 {
		t.Fatalf("typed MCP result=%#v want one success result", mcpResult)
	}
	if got := mcpResult.GetSuccess().GetResults()[0].GetText().GetText(); got != "./README.md\n./main.go" {
		t.Fatalf("tool result text=%q", got)
	}
	if got := decoded.GetSettings().GetModelConfig().GetBase(); got != "claude-4-5-sonnet" {
		t.Fatalf("base model=%q want claude-4-5-sonnet", got)
	}
	if got := decoded.GetSettings().GetModelConfig().GetCliAgent(); got != identifier {
		t.Fatalf("cli agent model=%q want %q", got, identifier)
	}
	if got := decoded.GetSettings().GetModelConfig().GetComputerUseAgent(); got != computerUseModel {
		t.Fatalf("computer use model=%q want %q", got, computerUseModel)
	}
	if got := decoded.GetSettings().GetModelConfig().GetCoding(); got != "" {
		t.Fatalf("coding model=%q want empty", got)
	}
	if got := decoded.GetInput().GetContext().GetDirectory().GetPwd(); got != "/repo" {
		t.Fatalf("pwd=%q want /repo", got)
	}
}

func TestBuildRequestSettings_AdvertisesOnlyImplementedCapabilities(t *testing.T) {
	settings := buildRequestSettings(upstream.UpstreamRequest{}, false)
	wantTools := []warpapi.ToolType{
		warpapi.ToolType_GREP,
		warpapi.ToolType_FILE_GLOB,
		warpapi.ToolType_FILE_GLOB_V2,
		warpapi.ToolType_CALL_MCP_TOOL,
		warpapi.ToolType_RUN_SHELL_COMMAND,
		warpapi.ToolType_WRITE_TO_LONG_RUNNING_SHELL_COMMAND,
		warpapi.ToolType_READ_SHELL_COMMAND_OUTPUT,
		warpapi.ToolType_READ_FILES,
		warpapi.ToolType_APPLY_FILE_DIFFS,
	}
	if got := settings.GetSupportedTools(); !slices.Equal(got, wantTools) {
		t.Fatalf("supported tools=%v want %v", got, wantTools)
	}
	if settings.GetSupportsTodosUi() || settings.GetSupportsStartedChildTaskMessage() ||
		settings.GetSupportsSuggestPrompt() || settings.GetSupportsV4AFileDiffs() {
		t.Fatalf("unsupported capabilities advertised: %#v", settings)
	}
	for _, tool := range settings.GetSupportedCliAgentTools() {
		if tool == warpapi.ToolType_SEARCH_CODEBASE {
			t.Fatal("SEARCH_CODEBASE is advertised without a response decoder")
		}
	}
}

func TestBuildRequestBytes_FinalPromptIsAuthoritative(t *testing.T) {
	finalPrompt := "Instructions:\n- stay concise\n\nraw request\n\n<tool_gate>\nDo not call tools.\n</tool_gate>"
	req := upstream.UpstreamRequest{
		Model:   "auto-open",
		Prompt:  finalPrompt,
		NoTools: true,
		Messages: []prompt.Message{{
			Role:    "user",
			Content: prompt.MessageContent{Text: "raw request"},
		}},
		System: []prompt.SystemItem{{Text: "stay concise"}},
	}

	query, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if query != finalPrompt {
		t.Fatalf("query=%q want finalized prompt", query)
	}
	if strings.Count(query, "stay concise") != 1 || strings.Count(query, "<tool_gate>") != 1 {
		t.Fatalf("final prompt was duplicated or discarded: %q", query)
	}

	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	inputs := decoded.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != finalPrompt {
		t.Fatalf("wire query=%#v want finalized prompt", inputs)
	}
	settings := decoded.GetSettings()
	if !slices.Equal(settings.GetSupportedTools(), warpTextOnlyToolFence) || !slices.Equal(settings.GetSupportedCliAgentTools(), warpTextOnlyToolFence) {
		t.Fatalf("no-tools settings=%v/%v want protocol fence %v", settings.GetSupportedTools(), settings.GetSupportedCliAgentTools(), warpTextOnlyToolFence)
	}
	if settings.GetSupportsParallelToolCalls() || settings.GetSupportsCreateFiles() ||
		settings.GetSupportsLongRunningCommands() || settings.GetSupportsV4AFileDiffs() ||
		settings.GetWebSearchEnabled() || settings.GetWebContextRetrievalEnabled() {
		t.Fatalf("tool capabilities remained enabled: %#v", settings)
	}
	if decoded.GetMcpContext() != nil {
		t.Fatalf("no-tools request unexpectedly included MCP context: %#v", decoded.GetMcpContext())
	}
}

func TestBuildRequestBytes_MalformedToolsDisableToolCapabilities(t *testing.T) {
	req := upstream.UpstreamRequest{
		Prompt: "answer directly",
		Model:  "auto-open",
		Tools: []interface{}{
			"invalid",
			map[string]interface{}{"name": "  "},
		},
	}

	_, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	settings := decoded.GetSettings()
	if settings.GetSupportsParallelToolCalls() || settings.GetSupportsCreateFiles() || settings.GetWebSearchEnabled() {
		t.Fatalf("malformed tools left capabilities enabled: %#v", settings)
	}
	if decoded.GetMcpContext() != nil {
		t.Fatalf("malformed tools produced MCP context: %#v", decoded.GetMcpContext())
	}
}

func TestBuildRequestBytes_CombinesTypedToolResultAndUserQuery(t *testing.T) {
	req := upstream.UpstreamRequest{
		Model:         "auto-open",
		ChatSessionID: "warp_conv_2",
		Messages: []prompt.Message{
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call_write", Name: "Write", Input: map[string]interface{}{"file_path": "main.go"}}}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call_write", Content: "written"}}}},
			{Role: "user", Content: prompt.MessageContent{Text: "continue"}},
		},
		Tools: []interface{}{map[string]interface{}{"name": "Write", "input_schema": map[string]interface{}{"type": "object"}}},
		WarpToolContexts: map[string]upstream.WarpToolContext{
			"call_write": {Type: "call_mcp_tool", Name: "Write", Input: `{"file_path":"main.go"}`},
		},
	}

	query, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}
	if query != "continue" {
		t.Fatalf("query=%q want continue", query)
	}
	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	inputs := decoded.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 2 || inputs[0].GetToolCallResult() == nil || inputs[1].GetUserQuery() == nil {
		t.Fatalf("inputs=%#v want typed tool result followed by user query", inputs)
	}
}

func TestBuildRequestBytes_EncodesNativeWarpToolResults(t *testing.T) {
	tests := []struct {
		name      string
		toolID    string
		toolName  string
		toolType  string
		toolInput string
		content   string
		check     func(*testing.T, *warpapi.Request_Input_ToolCallResult)
	}{
		{
			name:   "read files",
			toolID: "read_1", toolName: "Read", toolType: "read_files",
			toolInput: `{"file_path":"main.go"}`, content: "package main",
			check: func(t *testing.T, result *warpapi.Request_Input_ToolCallResult) {
				read := result.GetReadFiles().GetTextFilesSuccess().GetFiles()
				if len(read) != 1 || read[0].GetFilePath() != "main.go" || read[0].GetContent() != "package main" {
					t.Fatalf("read result=%#v", read)
				}
			},
		},
		{
			name:   "shell command",
			toolID: "shell_1", toolName: "Bash", toolType: "run_shell_command",
			toolInput: `{"command":"pwd"}`, content: "Exit code 2\n/repo",
			check: func(t *testing.T, result *warpapi.Request_Input_ToolCallResult) {
				shell := result.GetRunShellCommand()
				if shell.GetCommand() != "pwd" || shell.GetCommandFinished().GetExitCode() != 2 || shell.GetCommandFinished().GetOutput() != "Exit code 2\n/repo" {
					t.Fatalf("shell result=%#v", shell)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := upstream.UpstreamRequest{
				Model: "auto-open", ChatSessionID: "warp_conv",
				Messages:         []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: tt.toolID, Content: tt.content}}}}},
				WarpToolContexts: map[string]upstream.WarpToolContext{tt.toolID: {Type: tt.toolType, Name: tt.toolName, Input: tt.toolInput}},
			}
			_, payload, err := buildRequestBytes(req)
			if err != nil {
				t.Fatalf("buildRequestBytes error: %v", err)
			}
			var decoded warpapi.Request
			if err := proto.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			result := decoded.GetInput().GetUserInputs().GetInputs()[0].GetToolCallResult()
			if result.GetToolCallId() != tt.toolID {
				t.Fatalf("tool id=%q want %q", result.GetToolCallId(), tt.toolID)
			}
			tt.check(t, result)
		})
	}
}

func TestBuildRequestBytes_AutoOpenUsesDedicatedComputerUseModel(t *testing.T) {
	_, payload, err := buildRequestBytes(upstream.UpstreamRequest{
		Prompt: "open a shell",
		Model:  "auto-open",
	})
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}

	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request proto: %v", err)
	}
	modelConfig := decoded.GetSettings().GetModelConfig()
	if got := modelConfig.GetBase(); got != "auto-open" {
		t.Fatalf("base model=%q want auto-open", got)
	}
	if got := modelConfig.GetComputerUseAgent(); got != computerUseModel {
		t.Fatalf("computer use model=%q want %q", got, computerUseModel)
	}
	if got := modelConfig.GetCoding(); got != "" {
		t.Fatalf("coding model=%q want empty", got)
	}
}

func TestBuildRequestBytes_UsesAccountFeatureAgentModels(t *testing.T) {
	_, payload, err := buildRequestBytes(upstream.UpstreamRequest{
		Prompt:               "open a browser",
		Model:                "auto-open",
		WarpCliAgentModel:    "cli-agent-team-auto",
		WarpComputerUseModel: "computer-use-agent-team-auto",
	})
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}

	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request proto: %v", err)
	}
	modelConfig := decoded.GetSettings().GetModelConfig()
	if got := modelConfig.GetBase(); got != "auto-open" {
		t.Fatalf("base model=%q want auto-open", got)
	}
	if got := modelConfig.GetCliAgent(); got != "cli-agent-team-auto" {
		t.Fatalf("cli agent model=%q want cli-agent-team-auto", got)
	}
	if got := modelConfig.GetComputerUseAgent(); got != "computer-use-agent-team-auto" {
		t.Fatalf("computer use model=%q want computer-use-agent-team-auto", got)
	}
}

func TestEstimateInputTokens_OfficialProtoProfile(t *testing.T) {
	estimate, err := EstimateInputTokens("say hi", "gpt-4o", nil, nil, nil, false, "")
	if err != nil {
		t.Fatalf("EstimateInputTokens error: %v", err)
	}
	if estimate.Profile != "warp-official-proto" {
		t.Fatalf("profile=%q want warp-official-proto", estimate.Profile)
	}
	if estimate.Total <= 0 {
		t.Fatalf("expected positive total tokens, got %d", estimate.Total)
	}
}

func TestPreviewUserQuery_MatchesRequestBuilderConversationRules(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "list files"}},
	}
	system := []prompt.SystemItem{{Type: "text", Text: "Answer in Chinese."}}

	newPreview := PreviewUserQuery("", messages, system, "")
	if !strings.Contains(newPreview, "Instructions:") {
		t.Fatalf("new conversation preview missing instructions: %q", newPreview)
	}

	newQuery, newPayload, err := buildRequestBytes(upstream.UpstreamRequest{
		Model:    "auto-open",
		Messages: messages,
		System:   system,
	})
	if err != nil {
		t.Fatalf("build new request: %v", err)
	}
	if newQuery != newPreview {
		t.Fatalf("new query=%q want preview %q", newQuery, newPreview)
	}
	var newDecoded warpapi.Request
	if err := proto.Unmarshal(newPayload, &newDecoded); err != nil {
		t.Fatalf("decode new request: %v", err)
	}
	if got := newDecoded.GetMetadata().GetConversationId(); got != "" {
		t.Fatalf("new request conversation_id=%q want empty", got)
	}

	existingID := "conv_server_123"
	existingPreview := PreviewUserQuery("", messages, system, existingID)
	if strings.Contains(existingPreview, "Instructions:") {
		t.Fatalf("existing conversation preview should not repeat instructions: %q", existingPreview)
	}
	existingQuery, existingPayload, err := buildRequestBytes(upstream.UpstreamRequest{
		Model:         "auto-open",
		Messages:      messages,
		System:        system,
		ChatSessionID: existingID,
	})
	if err != nil {
		t.Fatalf("build existing request: %v", err)
	}
	if existingQuery != existingPreview {
		t.Fatalf("existing query=%q want preview %q", existingQuery, existingPreview)
	}
	var existingDecoded warpapi.Request
	if err := proto.Unmarshal(existingPayload, &existingDecoded); err != nil {
		t.Fatalf("decode existing request: %v", err)
	}
	if got := existingDecoded.GetMetadata().GetConversationId(); got != existingID {
		t.Fatalf("existing request conversation_id=%q want %q", got, existingID)
	}
}

func TestPreviewUserQuery_StatelessConversationPreservesHistory(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "My name is Ada."}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "Nice to meet you, Ada."}},
		{Role: "user", Content: prompt.MessageContent{Text: "What is my name?"}},
	}
	query := PreviewUserQuery("", messages, nil, "")
	for _, want := range []string{"My name is Ada.", "Nice to meet you, Ada.", "What is my name?"} {
		if !strings.Contains(query, want) {
			t.Fatalf("stateless query lost history %q: %q", want, query)
		}
	}
	if strings.Contains(PreviewUserQuery("", messages, nil, "warp_server_conversation"), "My name is Ada.") {
		t.Fatal("server-backed conversation should rely on Warp history rather than replaying it")
	}
}

func TestConvertTools_PreservesCustomMCPTools(t *testing.T) {
	t.Parallel()

	tools := []interface{}{
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "workspace_search",
				"description": strings.Repeat("search project symbols ", 40),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "term to search for",
						},
						"top_k": map[string]interface{}{
							"type": "integer",
						},
					},
				},
			},
		},
		map[string]interface{}{
			"name":        "Read",
			"description": "read file",
			"input_schema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
			},
		},
	}

	got := convertTools(tools)
	if len(got) != 2 {
		t.Fatalf("convertTools len=%d want=2 (%#v)", len(got), got)
	}
	if got[0].Name != "workspace_search" {
		t.Fatalf("custom tool name=%q want workspace_search", got[0].Name)
	}
	// A description is how the model decides when the tool applies, so it is
	// forwarded whole (surrounding whitespace aside). The old 512-character cut
	// removed the guidance and left a "...[truncated]" marker in its place.
	wantDescription := strings.TrimSpace(strings.Repeat("search project symbols ", 40))
	if got[0].Description != wantDescription {
		t.Fatalf("custom tool description was rewritten: got %d runes, want %d",
			len([]rune(got[0].Description)), len([]rune(wantDescription)))
	}
	if strings.Contains(got[0].Description, "[truncated]") {
		t.Fatalf("custom tool description was truncated: %q", got[0].Description)
	}
	props, ok := got[0].Schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("custom tool properties type=%T", got[0].Schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Fatalf("custom tool schema lost query property: %#v", got[0].Schema)
	}
	if got[1].Name != "Read" {
		t.Fatalf("builtin tool name=%q want Read", got[1].Name)
	}
}

func TestBuildWarpFileGlobV2Result_StreamsLines(t *testing.T) {
	result := buildWarpFileGlobV2Result(" /repo/a.go \n\n/repo/b.go\n", false)
	matches := result.GetSuccess().GetMatchedFiles()
	if len(matches) != 2 {
		t.Fatalf("matches=%d want=2", len(matches))
	}
	if got := matches[0].GetFilePath(); got != "/repo/a.go" {
		t.Fatalf("first path=%q", got)
	}
	if got := matches[1].GetFilePath(); got != "/repo/b.go" {
		t.Fatalf("second path=%q", got)
	}
}

func TestBuildRequestBytes_GroupsToolsInMCPServer(t *testing.T) {
	req := upstream.UpstreamRequest{
		Prompt: "use the tool",
		Model:  "auto-open",
		Tools: []interface{}{
			map[string]interface{}{
				"name":        "workspace_search",
				"description": "search project symbols",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{"type": "string"},
					},
				},
			},
		},
	}

	_, payload, err := buildRequestBytes(req)
	if err != nil {
		t.Fatalf("buildRequestBytes error: %v", err)
	}
	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request proto: %v", err)
	}
	servers := decoded.GetMcpContext().GetServers()
	if len(servers) != 1 {
		t.Fatalf("mcp servers=%d want 1", len(servers))
	}
	tools := servers[0].GetTools()
	if len(tools) != 1 {
		t.Fatalf("mcp tools=%d want 1", len(tools))
	}
	if got := tools[0].GetName(); got != "workspace_search" {
		t.Fatalf("tool name=%q want workspace_search", got)
	}
}
