package warp

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/goccy/go-json"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"orchids-api/internal/orchids"
	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
)

type InputTokenEstimate struct {
	Profile          string
	QueryTokens      int
	BasePromptTokens int
	HistoryTokens    int
	ToolResultTokens int
	ToolSchemaTokens int
	ToolCount        int
	Total            int
}

func buildRequestBytes(req upstream.UpstreamRequest) (string, []byte, error) {
	query := buildWarpUserQuery(req.Prompt, req.Messages, req.System, req.ChatSessionID)
	if strings.TrimSpace(query) == "" {
		return "", nil, fmt.Errorf("empty warp prompt")
	}

	disableWarpTools := req.NoTools || len(req.Tools) == 0
	modelConfig := buildRequestModelConfig(req)
	apiReq := warpapi.Request_builder{
		TaskContext: warpapi.Request_TaskContext_builder{}.Build(),
		Input:       buildRequestInput(query, req.Workdir),
		Settings:    buildRequestSettings(modelConfig, disableWarpTools),
		Metadata:    buildRequestMetadata(req.ChatSessionID),
	}.Build()
	if !disableWarpTools {
		mcpContext, err := buildMCPContext(req.Tools)
		if err != nil {
			return "", nil, err
		}
		apiReq.SetMcpContext(mcpContext)
	}

	payload, err := proto.Marshal(apiReq)
	if err != nil {
		return "", nil, err
	}
	return query, payload, nil
}

func extractMessageText(content prompt.MessageContent) string {
	if content.IsString() {
		return sanitizeUTF8(strings.TrimSpace(content.GetText()))
	}

	var parts []string
	for _, block := range content.GetBlocks() {
		if block.Type == "text" {
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, sanitizeUTF8(text))
			}
		}
	}
	return sanitizeUTF8(strings.TrimSpace(strings.Join(parts, "\n")))
}

func buildWarpUserQuery(promptText string, messages []prompt.Message, systemItems []prompt.SystemItem, conversationID string) string {
	query := latestWarpUserInput(messages)
	if query == "" && len(messages) == 0 {
		query = sanitizeUTF8(strings.TrimSpace(promptText))
	}
	if query == "" {
		query = sanitizeUTF8(strings.TrimSpace(promptText))
	}
	if query == "" {
		return ""
	}

	if shouldSendWarpConversationID(conversationID) {
		return query
	}
	systemText := renderWarpSystemInstructions(systemItems, messages)
	if systemText == "" {
		return query
	}
	return systemText + "\n\n" + query
}

func latestWarpUserInput(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Role))
		if role != "user" && role != "tool" {
			continue
		}
		if text := renderWarpUserMessageContent(messages[i].Content); text != "" {
			return text
		}
	}
	return ""
}

func renderWarpSystemInstructions(systemItems []prompt.SystemItem, messages []prompt.Message) string {
	var parts []string
	for _, item := range systemItems {
		if text := sanitizeUTF8(strings.TrimSpace(item.Text)); text != "" {
			parts = append(parts, text)
		}
	}
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "system") {
			continue
		}
		if text := extractMessageText(msg.Content); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Instructions:\n" + strings.Join(parts, "\n")
}

func renderWarpUserMessageContent(content prompt.MessageContent) string {
	if content.IsString() {
		return sanitizeUTF8(strings.TrimSpace(content.GetText()))
	}

	var parts []string
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := sanitizeUTF8(strings.TrimSpace(block.Text)); text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			payload := stringifyValue(block.Content)
			if payload == "" {
				payload = "{}"
			}
			label := strings.TrimSpace(block.ToolUseID)
			if label == "" {
				label = strings.TrimSpace(block.Name)
			}
			if label != "" {
				parts = append(parts, "Tool result ("+label+"):\n"+payload)
			} else {
				parts = append(parts, "Tool result:\n"+payload)
			}
		}
	}
	return sanitizeUTF8(strings.TrimSpace(strings.Join(parts, "\n\n")))
}

func sanitizeUTF8(text string) string {
	return strings.ToValidUTF8(text, "")
}

func stringifyValue(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return sanitizeUTF8(strings.TrimSpace(t))
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return sanitizeUTF8(fmt.Sprint(t))
		}
		return sanitizeUTF8(string(b))
	}
}

func PreviewUserQuery(promptText string, messages []prompt.Message, systemItems []prompt.SystemItem, conversationID string) string {
	return buildWarpUserQuery(promptText, messages, systemItems, conversationID)
}

func EstimateInputTokens(promptText, _ string, messages []prompt.Message, systemItems []prompt.SystemItem, tools []interface{}, disableWarpTools bool, conversationID string) (InputTokenEstimate, error) {
	query := buildWarpUserQuery(promptText, messages, systemItems, conversationID)
	queryTokens := tiktoken.EstimateTextTokens(query)
	toolSchemaTokens := 0
	toolCount := 0
	if !disableWarpTools {
		for _, tool := range convertTools(tools) {
			toolCount++
			toolSchemaTokens += tiktoken.EstimateTextTokens(tool.Name)
			toolSchemaTokens += tiktoken.EstimateTextTokens(tool.Description)
			if len(tool.Schema) > 0 {
				if raw, err := json.Marshal(tool.Schema); err == nil {
					toolSchemaTokens += tiktoken.EstimateTextTokens(string(raw))
				}
			}
		}
	}

	return InputTokenEstimate{
		Profile:          "warp-official-proto",
		QueryTokens:      queryTokens,
		BasePromptTokens: 0,
		HistoryTokens:    0,
		ToolResultTokens: 0,
		ToolSchemaTokens: toolSchemaTokens,
		ToolCount:        toolCount,
		Total:            queryTokens + toolSchemaTokens,
	}, nil
}

func DefaultModel() string {
	return defaultModel
}

func normalizeWarpModel(model string) string {
	canonical := canonicalModelID(model)
	if canonical == "" {
		return defaultModel
	}
	return canonical
}

func buildRequestModelConfig(req upstream.UpstreamRequest) AccountFeatureConfig {
	cfg := AccountFeatureConfig{
		BaseModel:             normalizeWarpModel(req.Model),
		CliAgentModel:         canonicalModelID(req.WarpCliAgentModel),
		ComputerUseAgentModel: canonicalModelID(req.WarpComputerUseModel),
	}
	if cfg.CliAgentModel == "" {
		cfg.CliAgentModel = identifier
	}
	if cfg.ComputerUseAgentModel == "" {
		cfg.ComputerUseAgentModel = computerUseModel
	}
	return cfg
}

func buildRequestInput(query, workdir string) *warpapi.Request_Input {
	agent := warpapi.AgentType_AGENT_TYPE_PRIMARY
	userQuery := warpapi.Request_Input_UserQuery_builder{
		Query:         stringPtr(query),
		Mode:          warpapi.UserQueryMode_builder{}.Build(),
		IntendedAgent: &agent,
	}.Build()
	userInput := warpapi.Request_Input_UserInputs_UserInput_builder{
		UserQuery: userQuery,
	}.Build()
	userInputs := warpapi.Request_Input_UserInputs_builder{
		Inputs: []*warpapi.Request_Input_UserInputs_UserInput{userInput},
	}.Build()
	return warpapi.Request_Input_builder{
		Context:    buildInputContext(workdir),
		UserInputs: userInputs,
	}.Build()
}

func buildInputContext(workdir string) *warpapi.InputContext {
	pwd := strings.TrimSpace(workdir)
	return warpapi.InputContext_builder{
		Directory: warpapi.InputContext_Directory_builder{
			Pwd:  stringPtr(pwd),
			Home: stringPtr(""),
		}.Build(),
		OperatingSystem: warpapi.InputContext_OperatingSystem_builder{
			Platform:     stringPtr(warpPlatformName()),
			Distribution: stringPtr(""),
		}.Build(),
		Shell: warpapi.InputContext_Shell_builder{
			Name:    stringPtr(defaultShellName()),
			Version: stringPtr(""),
		}.Build(),
		CurrentTime: timestamppb.Now(),
	}.Build()
}

func buildRequestSettings(modelConfig AccountFeatureConfig, disableTools bool) *warpapi.Request_Settings {
	cliAgentModel := firstNonEmptyModelID(modelConfig.CliAgentModel)
	if cliAgentModel == "" {
		cliAgentModel = identifier
	}
	computerAgentModel := firstNonEmptyModelID(modelConfig.ComputerUseAgentModel)
	if computerAgentModel == "" {
		computerAgentModel = computerUseModel
	}
	contextLimit := uint32(0)
	supportedTools := []warpapi.ToolType(nil)
	supportedCliAgentTools := []warpapi.ToolType(nil)
	if !disableTools {
		supportedTools = officialSupportedTools()
		supportedCliAgentTools = officialSupportedCliAgentTools()
	}
	autonomy := warpapi.AutonomyLevel_SUPERVISED
	isolation := warpapi.IsolationLevel_NONE
	return warpapi.Request_Settings_builder{
		ModelConfig: warpapi.Request_Settings_ModelConfig_builder{
			Base:                        stringPtr(normalizeWarpModel(modelConfig.BaseModel)),
			CliAgent:                    stringPtr(cliAgentModel),
			ComputerUseAgent:            stringPtr(computerAgentModel),
			BaseModelContextWindowLimit: &contextLimit,
		}.Build(),
		WebContextRetrievalEnabled:                 boolPtr(true),
		SupportsParallelToolCalls:                  boolPtr(true),
		UseAnthropicTextEditorTools:                boolPtr(false),
		PlanningEnabled:                            boolPtr(false),
		WarpDriveContextEnabled:                    boolPtr(false),
		SupportsCreateFiles:                        boolPtr(true),
		SupportedTools:                             supportedTools,
		SupportsLongRunningCommands:                boolPtr(true),
		ShouldPreserveFileContentInHistory:         boolPtr(true),
		SupportsTodosUi:                            boolPtr(true),
		SupportsLinkedCodeBlocks:                   boolPtr(false),
		SupportsStartedChildTaskMessage:            boolPtr(true),
		SupportsSuggestPrompt:                      boolPtr(true),
		SupportsReadImageFiles:                     boolPtr(false),
		SupportsReasoningMessage:                   boolPtr(true),
		AutonomyLevel:                              &autonomy,
		IsolationLevel:                             &isolation,
		WebSearchEnabled:                           boolPtr(true),
		SupportedCliAgentTools:                     supportedCliAgentTools,
		SupportsV4AFileDiffs:                       boolPtr(true),
		SupportsSummarizationViaMessageReplacement: boolPtr(false),
		SupportsBundledSkills:                      boolPtr(false),
		SupportsResearchAgent:                      boolPtr(false),
		SupportsOrchestrationV2:                    boolPtr(false),
	}.Build()
}

func buildRequestMetadata(conversationID string) *warpapi.Request_Metadata {
	builder := warpapi.Request_Metadata_builder{}
	if shouldSendWarpConversationID(conversationID) {
		builder.ConversationId = stringPtr(strings.TrimSpace(conversationID))
	}
	return builder.Build()
}

func shouldSendWarpConversationID(conversationID string) bool {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return false
	}
	return !strings.HasPrefix(conversationID, "chat_")
}

func officialSupportedTools() []warpapi.ToolType {
	return []warpapi.ToolType{
		warpapi.ToolType_GREP,
		warpapi.ToolType_FILE_GLOB,
		warpapi.ToolType_FILE_GLOB_V2,
		warpapi.ToolType_READ_MCP_RESOURCE,
		warpapi.ToolType_CALL_MCP_TOOL,
		warpapi.ToolType_INIT_PROJECT,
		warpapi.ToolType_OPEN_CODE_REVIEW,
		warpapi.ToolType_RUN_SHELL_COMMAND,
		warpapi.ToolType_SUGGEST_NEW_CONVERSATION,
		warpapi.ToolType_SUBAGENT,
		warpapi.ToolType_WRITE_TO_LONG_RUNNING_SHELL_COMMAND,
		warpapi.ToolType_READ_SHELL_COMMAND_OUTPUT,
		warpapi.ToolType_READ_DOCUMENTS,
		warpapi.ToolType_CREATE_DOCUMENTS,
		warpapi.ToolType_EDIT_DOCUMENTS,
		warpapi.ToolType_SUGGEST_PROMPT,
		warpapi.ToolType_READ_FILES,
		warpapi.ToolType_APPLY_FILE_DIFFS,
		warpapi.ToolType_SEARCH_CODEBASE,
	}
}

func officialSupportedCliAgentTools() []warpapi.ToolType {
	return []warpapi.ToolType{
		warpapi.ToolType_WRITE_TO_LONG_RUNNING_SHELL_COMMAND,
		warpapi.ToolType_READ_SHELL_COMMAND_OUTPUT,
		warpapi.ToolType_GREP,
		warpapi.ToolType_FILE_GLOB,
		warpapi.ToolType_FILE_GLOB_V2,
		warpapi.ToolType_READ_FILES,
		warpapi.ToolType_SEARCH_CODEBASE,
	}
}

func buildMCPContext(tools []interface{}) (*warpapi.Request_MCPContext, error) {
	converted := convertTools(tools)
	if len(converted) == 0 {
		return nil, nil
	}

	mcpTools := make([]*warpapi.Request_MCPContext_MCPTool, 0, len(converted))
	for _, tool := range converted {
		var schema *structpb.Struct
		if len(tool.Schema) > 0 {
			st, err := structpb.NewStruct(tool.Schema)
			if err != nil {
				return nil, err
			}
			schema = st
		}
		mcpTools = append(mcpTools, warpapi.Request_MCPContext_MCPTool_builder{
			Name:        stringPtr(tool.Name),
			Description: stringPtr(tool.Description),
			InputSchema: schema,
		}.Build())
	}
	server := warpapi.Request_MCPContext_MCPServer_builder{
		Name:        stringPtr("orchids"),
		Description: stringPtr("Tools declared by the client request"),
		Id:          stringPtr("orchids-request-tools"),
		Tools:       mcpTools,
	}.Build()
	return warpapi.Request_MCPContext_builder{
		Servers: []*warpapi.Request_MCPContext_MCPServer{server},
	}.Build(), nil
}

func warpPlatformName() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

func defaultShellName() string {
	switch runtime.GOOS {
	case "windows":
		return "powershell"
	default:
		return "zsh"
	}
}

func stringPtr(value string) *string {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

type toolDef struct {
	Name        string
	Description string
	Schema      map[string]interface{}
}

const (
	maxWarpToolCount         = 32
	maxWarpToolDescLen       = 512
	maxWarpToolSchemaJSONLen = 4096
)

var warpBuiltinToolNames = map[string]struct{}{
	"Bash":      {},
	"Read":      {},
	"Edit":      {},
	"Write":     {},
	"Glob":      {},
	"Grep":      {},
	"TodoWrite": {},
}

var warpToolAllowedProps = map[string]map[string]struct{}{
	"Bash": {
		"command":           {},
		"description":       {},
		"run_in_background": {},
		"timeout":           {},
	},
	"Read": {
		"file_path": {},
		"offset":    {},
		"limit":     {},
		"pages":     {},
	},
	"Edit": {
		"file_path":   {},
		"old_string":  {},
		"new_string":  {},
		"replace_all": {},
	},
	"Write": {
		"file_path": {},
		"content":   {},
	},
	"Glob": {
		"pattern": {},
		"path":    {},
	},
	"Grep": {
		"pattern":     {},
		"path":        {},
		"glob":        {},
		"type":        {},
		"output_mode": {},
		"-i":          {},
		"multiline":   {},
		"head_limit":  {},
		"offset":      {},
		"context":     {},
	},
	"TodoWrite": {
		"todos": {},
	},
}

func isWarpBuiltinTool(name string) bool {
	_, ok := warpBuiltinToolNames[name]
	return ok
}

func convertTools(tools []interface{}) []toolDef {
	if len(tools) == 0 {
		return nil
	}

	defs := make([]toolDef, 0, len(tools))
	seen := make(map[string]struct{})
	for _, raw := range tools {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, description, schema := extractWarpToolSpecFields(m)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		canonicalName := orchids.NormalizeToolNameFallback(name)
		key := strings.ToLower(name)
		if isWarpBuiltinTool(canonicalName) {
			key = "builtin:" + strings.ToLower(canonicalName)
		}
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		schema = compactWarpSchemaForTool(canonicalName, schema)
		defs = append(defs, toolDef{
			Name:        name,
			Description: compactWarpDescription(description),
			Schema:      schema,
		})
		if len(defs) >= maxWarpToolCount {
			break
		}
	}
	return defs
}

func extractWarpToolSpecFields(tool map[string]interface{}) (string, string, map[string]interface{}) {
	if tool == nil {
		return "", "", nil
	}

	var name string
	var description string
	var schema map[string]interface{}

	if fn, ok := tool["function"].(map[string]interface{}); ok {
		if v, ok := fn["name"].(string); ok {
			name = v
		}
		if v, ok := fn["description"].(string); ok {
			description = v
		}
		schema = schemaMap(fn["parameters"])
		if schema == nil {
			schema = schemaMap(fn["input_schema"])
		}
	}
	if name == "" {
		if v, ok := tool["name"].(string); ok {
			name = v
		}
	}
	if description == "" {
		if v, ok := tool["description"].(string); ok {
			description = v
		}
	}
	if schema == nil {
		schema = schemaMap(tool["input_schema"])
	}
	if schema == nil {
		schema = schemaMap(tool["parameters"])
	}
	return name, description, schema
}

func schemaMap(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func compactWarpDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	const suffix = "...[truncated]"
	runes := []rune(description)
	if len(runes) <= maxWarpToolDescLen {
		return description
	}
	keep := maxWarpToolDescLen - len([]rune(suffix))
	if keep <= 0 {
		return suffix
	}
	return string(runes[:keep]) + suffix
}

func compactWarpSchemaForTool(name string, schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	cleaned := cleanWarpSchema(schema)
	if cleaned == nil {
		return nil
	}
	filtered := filterWarpSchemaProperties(name, cleaned)
	if filtered == nil {
		return nil
	}
	cleaned = filtered
	if warpSchemaJSONLen(cleaned) <= maxWarpToolSchemaJSONLen {
		return cleaned
	}
	stripped := stripWarpSchemaDescriptions(cleaned)
	if warpSchemaJSONLen(stripped) <= maxWarpToolSchemaJSONLen {
		return stripped
	}
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func filterWarpSchemaProperties(name string, schema map[string]interface{}) map[string]interface{} {
	allowed, ok := warpToolAllowedProps[name]
	if !ok || schema == nil {
		return schema
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		return schema
	}

	filtered := make(map[string]interface{}, len(props))
	for key, value := range props {
		if _, keep := allowed[key]; keep {
			filtered[key] = value
		}
	}

	out := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		switch key {
		case "properties":
			out[key] = filtered
		case "required":
			raw, ok := value.([]interface{})
			if !ok {
				out[key] = value
				continue
			}
			req := make([]interface{}, 0, len(raw))
			for _, item := range raw {
				propName, _ := item.(string)
				if _, keep := allowed[propName]; keep {
					req = append(req, item)
				}
			}
			if len(req) > 0 {
				out[key] = req
			}
		default:
			out[key] = value
		}
	}
	return out
}

func cleanWarpSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	sanitized := map[string]interface{}{}
	for _, key := range []string{"type", "description", "properties", "required", "enum", "items"} {
		if v, ok := schema[key]; ok {
			sanitized[key] = v
		}
	}
	if props, ok := sanitized["properties"].(map[string]interface{}); ok {
		cleanProps := map[string]interface{}{}
		for name, prop := range props {
			cleanProps[name] = cleanWarpSchemaValue(prop)
		}
		sanitized["properties"] = cleanProps
	}
	if items, ok := sanitized["items"]; ok {
		sanitized["items"] = cleanWarpSchemaValue(items)
	}
	return sanitized
}

func cleanWarpSchemaValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return cleanWarpSchema(v)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, cleanWarpSchemaValue(item))
		}
		return out
	default:
		return value
	}
}

func stripWarpSchemaDescriptions(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	out := make(map[string]interface{}, len(schema))
	for k, v := range schema {
		if strings.EqualFold(k, "description") || strings.EqualFold(k, "title") {
			continue
		}
		out[k] = stripWarpSchemaDescriptionsValue(v)
	}
	return out
}

func stripWarpSchemaDescriptionsValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return stripWarpSchemaDescriptions(v)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, stripWarpSchemaDescriptionsValue(item))
		}
		return out
	default:
		return value
	}
}

func warpSchemaJSONLen(schema map[string]interface{}) int {
	if schema == nil {
		return 0
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return 0
	}
	return len(raw)
}
