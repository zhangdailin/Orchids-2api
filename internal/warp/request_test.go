package warp

import (
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestBuildRequestBytes_UsesOfficialProtoRequest(t *testing.T) {
	req := upstream.UpstreamRequest{
		Prompt:  "ignored because messages are present",
		Model:   "claude-4-5-sonnet",
		Workdir: "/repo",
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
	if !strings.Contains(query, "Tool result (call_1):") {
		t.Fatalf("query missing tool result transcript: %q", query)
	}

	var decoded warpapi.Request
	if err := proto.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode request proto: %v", err)
	}
	if decoded.GetInput().WhichType() != warpapi.Request_Input_UserInputs_case {
		t.Fatalf("input type=%v want user_inputs", decoded.GetInput().WhichType())
	}
	inputs := decoded.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 {
		t.Fatalf("user inputs=%d want 1", len(inputs))
	}
	userQuery := inputs[0].GetUserQuery()
	if userQuery == nil {
		t.Fatal("user input missing user query")
	}
	if got := userQuery.GetQuery(); got != query {
		t.Fatalf("query=%q want %q", got, query)
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
	if !strings.HasSuffix(got[0].Description, "...[truncated]") {
		t.Fatalf("custom tool description=%q want truncated suffix", got[0].Description)
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
