package warp

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/toolname"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

type InputTokenEstimate struct {
	Profile          string
	BasePromptTokens int
	HistoryTokens    int
	ToolResultTokens int
	ToolSchemaTokens int
	Total            int
}

var warpExitCodePattern = regexp.MustCompile(`(?i)\bexit(?:\s+code|\s+status)?\s*[:=]?\s*(-?\d+)\b`)
var warpGrepLinePattern = regexp.MustCompile(`^(.+?):(\d+)(?::|-)`)

// A request without a server-issued Warp conversation ID is stateless at the
// upstream. Keep enough transcript to preserve OpenAI/Claude multi-turn
// semantics instead of silently forwarding only the last user message.
const warpStatelessHistoryMaxChars = 48 * 1024

func buildRequestBytes(req upstream.UpstreamRequest) (string, []byte, error) {
	query := buildWarpUserQuery(req.Prompt, req.Messages, req.System, req.ChatSessionID)
	tools := convertTools(req.Tools)
	input, inputCount := buildRequestInput(query, req.Messages, req.Workdir, req.WarpToolContexts, tools)
	if strings.TrimSpace(query) == "" && inputCount == 0 {
		return "", nil, fmt.Errorf("empty warp prompt")
	}

	disableWarpTools := req.NoTools || len(tools) == 0
	taskContext, err := buildWarpTaskContext(req.WarpTaskContext)
	if err != nil {
		return "", nil, err
	}
	apiReq := warpapi.Request_builder{
		TaskContext: taskContext,
		Input:       input,
		Settings:    buildRequestSettings(req, disableWarpTools),
		Metadata:    buildRequestMetadata(req.ChatSessionID),
	}.Build()
	if !disableWarpTools {
		mcpContext, err := buildMCPContext(tools)
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

func buildWarpTaskContext(encoded []byte) (*warpapi.Request_TaskContext, error) {
	context := warpapi.Request_TaskContext_builder{}.Build()
	if len(encoded) == 0 {
		return context, nil
	}
	if err := proto.Unmarshal(encoded, context); err != nil {
		return nil, fmt.Errorf("decode Warp task context: %w", err)
	}
	return context, nil
}

func extractMessageText(content prompt.MessageContent) string {
	return joinWarpTextContent(content, "\n")
}

func buildWarpUserQuery(promptText string, messages []prompt.Message, systemItems []prompt.SystemItem, conversationID string) string {
	// Prompt is the handler's finalized query. It may contain safety gates or a
	// recovery instruction that intentionally replaces the raw client message.
	if query := sanitizeUTF8(strings.TrimSpace(promptText)); query != "" {
		return query
	}
	if shouldSendWarpConversationID(conversationID) {
		return latestWarpUserInput(messages)
	}
	return renderWarpStatelessTranscript(messages, systemItems)
}

func renderWarpStatelessTranscript(messages []prompt.Message, systemItems []prompt.SystemItem) string {
	parts := make([]string, 0, len(messages)+1)
	if systemText := renderWarpSystemInstructions(systemItems, messages); systemText != "" {
		parts = append(parts, systemText)
	}
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			continue // included above so it is never duplicated
		}
		if rendered := renderWarpTranscriptMessage(message); rendered != "" {
			parts = append(parts, rendered)
		}
	}
	if len(parts) == 0 {
		return ""
	}

	// Prefer the latest turns when an API client sends a very long stateless
	// transcript. System instructions remain at the front as an invariant.
	systemPart := ""
	start := 0
	if strings.HasPrefix(parts[0], "Instructions:") {
		systemPart, start = parts[0], 1
	}
	selected := make([]string, 0, len(parts)-start)
	used := len(systemPart)
	for i := len(parts) - 1; i >= start; i-- {
		part := parts[i]
		if used+len(part)+2 > warpStatelessHistoryMaxChars && len(selected) > 0 {
			break
		}
		selected = append(selected, part)
		used += len(part) + 2
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	if len(selected) < len(parts)-start {
		selected = append([]string{"[Earlier conversation omitted for length]"}, selected...)
	}
	if systemPart != "" {
		selected = append([]string{systemPart}, selected...)
	}
	return strings.Join(selected, "\n\n")
}

func renderWarpTranscriptMessage(message prompt.Message) string {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role == "" {
		role = "user"
	}
	if message.Content.IsString() {
		if text := sanitizeUTF8(strings.TrimSpace(message.Content.GetText())); text != "" {
			return role + ":\n" + text
		}
		return ""
	}
	parts := make([]string, 0, len(message.Content.GetBlocks()))
	for _, block := range message.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := sanitizeUTF8(strings.TrimSpace(block.Text)); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name != "" {
				parts = append(parts, fmt.Sprintf("tool call %s (%s)", name, stringifyValue(block.Input)))
			}
		case "tool_result":
			result := stringifyValue(block.Content)
			if result != "" {
				parts = append(parts, "tool result: "+result)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return role + ":\n" + strings.Join(parts, "\n")
}

func latestWarpUserInput(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Role))
		if role != "user" && role != "tool" {
			continue
		}
		return renderWarpUserMessageContent(messages[i].Content)
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
	return joinWarpTextContent(content, "\n\n")
}

func joinWarpTextContent(content prompt.MessageContent, separator string) string {
	if content.IsString() {
		return sanitizeUTF8(strings.TrimSpace(content.GetText()))
	}

	var parts []string
	for _, block := range content.GetBlocks() {
		if block.Type != "text" {
			continue
		}
		if text := sanitizeUTF8(strings.TrimSpace(block.Text)); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, separator)
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
	toolResultTokens := 0
	for _, block := range latestWarpToolResultBlocks(messages) {
		toolResultTokens += tiktoken.EstimateTextTokens(stringifyValue(block.Content))
	}
	toolSchemaTokens := 0
	if !disableWarpTools {
		for _, tool := range convertTools(tools) {
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
		BasePromptTokens: 0,
		HistoryTokens:    0,
		ToolResultTokens: toolResultTokens,
		ToolSchemaTokens: toolSchemaTokens,
		Total:            queryTokens + toolResultTokens + toolSchemaTokens,
	}, nil
}

func DefaultModel() string {
	return defaultModel
}

func normalizeWarpModel(model string) string {
	canonical := NormalizeModelID(model)
	if canonical == "" {
		return defaultModel
	}
	return canonical
}

func buildRequestInput(query string, messages []prompt.Message, workdir string, toolContexts map[string]upstream.WarpToolContext, tools []toolDef) (*warpapi.Request_Input, int) {
	resultBlocks := latestWarpToolResultBlocks(messages)
	inputs := make([]*warpapi.Request_Input_UserInputs_UserInput, 0, len(resultBlocks)+1)
	var declaredTools map[string]struct{}
	if len(resultBlocks) > 0 {
		declaredTools = make(map[string]struct{}, len(tools))
		for _, tool := range tools {
			declaredTools[strings.ToLower(strings.TrimSpace(tool.Name))] = struct{}{}
		}
	}
	for _, block := range resultBlocks {
		if result := buildWarpToolResult(block, messages, toolContexts, declaredTools); result != nil {
			inputs = append(inputs, warpapi.Request_Input_UserInputs_UserInput_builder{ToolCallResult: result}.Build())
		}
	}
	if strings.TrimSpace(query) != "" {
		inputs = append(inputs, buildWarpUserQueryInput(query))
	}
	return warpapi.Request_Input_builder{
		Context: buildInputContext(workdir),
		UserInputs: warpapi.Request_Input_UserInputs_builder{
			Inputs: inputs,
		}.Build(),
	}.Build(), len(inputs)
}

func buildWarpUserQueryInput(query string) *warpapi.Request_Input_UserInputs_UserInput {
	agent := warpapi.AgentType_AGENT_TYPE_PRIMARY
	userQuery := warpapi.Request_Input_UserQuery_builder{
		Query:         stringPtr(query),
		Mode:          warpapi.UserQueryMode_builder{}.Build(),
		IntendedAgent: &agent,
	}.Build()
	return warpapi.Request_Input_UserInputs_UserInput_builder{
		UserQuery: userQuery,
	}.Build()
}

func latestWarpToolResultBlocks(messages []prompt.Message) []prompt.ContentBlock {
	var reversed []prompt.ContentBlock
	foundPendingInput := false
	seen := make(map[string]struct{})
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Role))
		if role != "user" && role != "tool" {
			if foundPendingInput {
				break
			}
			continue
		}
		foundPendingInput = true
		if messages[i].Content.IsString() {
			continue
		}
		blocks := messages[i].Content.GetBlocks()
		for j := len(blocks) - 1; j >= 0; j-- {
			block := blocks[j]
			id := strings.TrimSpace(block.ToolUseID)
			if block.Type != "tool_result" || id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			reversed = append(reversed, block)
		}
	}
	results := make([]prompt.ContentBlock, len(reversed))
	for i := range reversed {
		results[len(reversed)-1-i] = reversed[i]
	}
	return results
}

func buildWarpToolResult(block prompt.ContentBlock, messages []prompt.Message, toolContexts map[string]upstream.WarpToolContext, declaredTools map[string]struct{}) *warpapi.Request_Input_ToolCallResult {
	id := strings.TrimSpace(block.ToolUseID)
	if id == "" {
		return nil
	}
	ctx := toolContexts[id]
	if ctx.Name == "" || ctx.Input == "" {
		name, input := findWarpToolUse(messages, id)
		if ctx.Name == "" {
			ctx.Name = name
		}
		if ctx.Input == "" {
			ctx.Input = input
		}
	}
	payload := stringifyValue(block.Content)
	toolType := strings.ToLower(strings.TrimSpace(ctx.Type))
	if toolType == "" {
		if _, ok := declaredTools[strings.ToLower(strings.TrimSpace(ctx.Name))]; ok {
			toolType = "call_mcp_tool"
		}
	}
	builder := warpapi.Request_Input_ToolCallResult_builder{ToolCallId: stringPtr(id)}
	switch toolType {
	case "run_shell_command", "run_command":
		builder.RunShellCommand = buildWarpShellResult(ctx.Input, payload, block.IsError)
	case "write_to_long_running_shell_command":
		builder.WriteToLongRunningShellCommand = buildWarpWriteShellResult(payload, block.IsError)
	case "read_shell_command_output":
		builder.ReadShellCommandOutput = buildWarpReadShellOutputResult(ctx.Input, payload, block.IsError)
	case "read_files", "read_file":
		builder.ReadFiles = buildWarpReadFilesResult(ctx.Input, payload, block.IsError)
	case "apply_file_diffs", "edit_file", "write_file":
		builder.ApplyFileDiffs = buildWarpApplyDiffsResult(payload, block.IsError)
	case "file_glob":
		builder.FileGlob = buildWarpFileGlobResult(payload, block.IsError)
	case "file_glob_v2":
		builder.FileGlobV2 = buildWarpFileGlobV2Result(payload, block.IsError)
	case "grep":
		builder.Grep = buildWarpGrepResult(payload, block.IsError)
	default:
		builder.CallMcpTool = buildWarpMCPToolResult(payload, block.IsError)
	}
	return builder.Build()
}

func findWarpToolUse(messages []prompt.Message, id string) (string, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Content.IsString() {
			continue
		}
		blocks := messages[i].Content.GetBlocks()
		for j := len(blocks) - 1; j >= 0; j-- {
			block := blocks[j]
			if block.Type == "tool_use" && strings.TrimSpace(block.ID) == id {
				return strings.TrimSpace(block.Name), stringifyValue(block.Input)
			}
		}
	}
	return "", ""
}

func buildWarpMCPToolResult(payload string, isError bool) *warpapi.CallMCPToolResult {
	if isError {
		return warpapi.CallMCPToolResult_builder{Error: warpapi.CallMCPToolResult_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	text := warpapi.CallMCPToolResult_Success_Result_Text_builder{Text: stringPtr(payload)}.Build()
	result := warpapi.CallMCPToolResult_Success_Result_builder{Text: text}.Build()
	return warpapi.CallMCPToolResult_builder{Success: warpapi.CallMCPToolResult_Success_builder{Results: []*warpapi.CallMCPToolResult_Success_Result{result}}.Build()}.Build()
}

func buildWarpShellResult(input, payload string, isError bool) *warpapi.RunShellCommandResult {
	command := jsonStringField(input, "command", "cmd")
	exitCode := warpShellExitCode(payload, isError)
	finished := warpapi.ShellCommandFinished_builder{Output: stringPtr(payload), ExitCode: &exitCode}.Build()
	return warpapi.RunShellCommandResult_builder{Command: stringPtr(command), CommandFinished: finished}.Build()
}

func buildWarpWriteShellResult(payload string, isError bool) *warpapi.WriteToLongRunningShellCommandResult {
	exitCode := warpShellExitCode(payload, isError)
	finished := warpapi.ShellCommandFinished_builder{Output: stringPtr(payload), ExitCode: &exitCode}.Build()
	return warpapi.WriteToLongRunningShellCommandResult_builder{CommandFinished: finished}.Build()
}

func buildWarpReadShellOutputResult(input, payload string, isError bool) *warpapi.ReadShellCommandOutputResult {
	exitCode := warpShellExitCode(payload, isError)
	command := jsonStringField(input, "command")
	commandID := jsonStringField(input, "command_id")
	finished := warpapi.ShellCommandFinished_builder{Output: stringPtr(payload), ExitCode: &exitCode, CommandId: stringPtr(commandID)}.Build()
	return warpapi.ReadShellCommandOutputResult_builder{Command: stringPtr(command), CommandFinished: finished}.Build()
}

func warpShellExitCode(payload string, isError bool) int32 {
	if match := warpExitCodePattern.FindStringSubmatch(payload); len(match) == 2 {
		if value, err := strconv.ParseInt(match[1], 10, 32); err == nil {
			return int32(value)
		}
	}
	if isError {
		return 1
	}
	return 0
}

func buildWarpReadFilesResult(input, payload string, isError bool) *warpapi.ReadFilesResult {
	if isError {
		return warpapi.ReadFilesResult_builder{Error: warpapi.ReadFilesResult_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	path := jsonStringField(input, "file_path", "path")
	file := warpapi.FileContent_builder{FilePath: stringPtr(path), Content: stringPtr(payload)}.Build()
	return warpapi.ReadFilesResult_builder{TextFilesSuccess: warpapi.ReadFilesResult_TextFilesSuccess_builder{Files: []*warpapi.FileContent{file}}.Build()}.Build()
}

func buildWarpApplyDiffsResult(payload string, isError bool) *warpapi.ApplyFileDiffsResult {
	if isError {
		return warpapi.ApplyFileDiffsResult_builder{Error: warpapi.ApplyFileDiffsResult_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	return warpapi.ApplyFileDiffsResult_builder{Success: warpapi.ApplyFileDiffsResult_Success_builder{}.Build()}.Build()
}

func buildWarpFileGlobResult(payload string, isError bool) *warpapi.FileGlobResult {
	if isError {
		return warpapi.FileGlobResult_builder{Error: warpapi.FileGlobResult_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	return warpapi.FileGlobResult_builder{Success: warpapi.FileGlobResult_Success_builder{MatchedFiles: stringPtr(payload)}.Build()}.Build()
}

func buildWarpFileGlobV2Result(payload string, isError bool) *warpapi.FileGlobV2Result {
	if isError {
		return warpapi.FileGlobV2Result_builder{Error: warpapi.FileGlobV2Result_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	matches := make([]*warpapi.FileGlobV2Result_Success_FileGlobMatch, 0)
	for _, line := range strings.Split(payload, "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		matches = append(matches, warpapi.FileGlobV2Result_Success_FileGlobMatch_builder{FilePath: stringPtr(path)}.Build())
	}
	return warpapi.FileGlobV2Result_builder{Success: warpapi.FileGlobV2Result_Success_builder{MatchedFiles: matches}.Build()}.Build()
}

func buildWarpGrepResult(payload string, isError bool) *warpapi.GrepResult {
	if isError {
		return warpapi.GrepResult_builder{Error: warpapi.GrepResult_Error_builder{Message: stringPtr(payload)}.Build()}.Build()
	}
	type fileLines struct {
		path  string
		lines []uint32
	}
	ordered := make([]fileLines, 0)
	indexes := make(map[string]int)
	for _, line := range strings.Split(payload, "\n") {
		match := warpGrepLinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 3 {
			continue
		}
		n, err := strconv.ParseUint(match[2], 10, 32)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(match[1])
		index, ok := indexes[path]
		if !ok {
			index = len(ordered)
			indexes[path] = index
			ordered = append(ordered, fileLines{path: path})
		}
		ordered[index].lines = append(ordered[index].lines, uint32(n))
	}
	files := make([]*warpapi.GrepResult_Success_GrepFileMatch, 0, len(ordered))
	for _, file := range ordered {
		lines := make([]*warpapi.GrepResult_Success_GrepFileMatch_GrepLineMatch, 0, len(file.lines))
		for _, n := range file.lines {
			lineNumber := n
			lines = append(lines, warpapi.GrepResult_Success_GrepFileMatch_GrepLineMatch_builder{LineNumber: &lineNumber}.Build())
		}
		files = append(files, warpapi.GrepResult_Success_GrepFileMatch_builder{FilePath: stringPtr(file.path), MatchedLines: lines}.Build())
	}
	return warpapi.GrepResult_builder{Success: warpapi.GrepResult_Success_builder{MatchedFiles: files}.Build()}.Build()
}

func jsonStringField(raw string, keys ...string) string {
	var value map[string]interface{}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return ""
	}
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func buildInputContext(workdir string) *warpapi.InputContext {
	pwd := strings.TrimSpace(workdir)
	return warpapi.InputContext_builder{
		Directory: warpapi.InputContext_Directory_builder{
			Pwd:  stringPtr(pwd),
			Home: stringPtr(""),
		}.Build(),
		OperatingSystem: warpapi.InputContext_OperatingSystem_builder{
			Platform:     stringPtr(warpOSCategory()),
			Distribution: stringPtr(""),
		}.Build(),
		Shell: warpapi.InputContext_Shell_builder{
			Name:    stringPtr(defaultShellName()),
			Version: stringPtr(""),
		}.Build(),
		CurrentTime: timestamppb.Now(),
	}.Build()
}

func buildRequestSettings(req upstream.UpstreamRequest, disableTools bool) *warpapi.Request_Settings {
	cliAgentModel := NormalizeModelID(req.WarpCliAgentModel)
	if cliAgentModel == "" {
		cliAgentModel = identifier
	}
	computerAgentModel := NormalizeModelID(req.WarpComputerUseModel)
	if computerAgentModel == "" {
		computerAgentModel = computerUseModel
	}
	contextLimit := uint32(0)
	// Warp defines an empty supported_tools list as "any tool", not "no tools".
	// Always send the bounded official lists. Per-request denial is enforced by
	// the handler's prompt gate and response-side hard gate.
	toolsEnabled := !disableTools
	parallelTools := toolsEnabled
	if req.ParallelToolCalls != nil {
		parallelTools = toolsEnabled && *req.ParallelToolCalls
	} else if choice, ok := req.ToolChoice.(map[string]interface{}); ok {
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok && disabled {
			parallelTools = false
		}
	}
	supportedTools := officialSupportedTools
	supportedCliTools := officialSupportedCliAgentTools
	if disableTools {
		// Warp treats an empty list as a wildcard. Use a non-executable passive
		// capability as a protocol fence while keeping supports_suggest_prompt
		// false and enforcing the explicit tool gate in the user query.
		supportedTools = warpTextOnlyToolFence
		supportedCliTools = warpTextOnlyToolFence
	}
	autonomy := warpapi.AutonomyLevel_SUPERVISED
	isolation := warpapi.IsolationLevel_NONE
	return warpapi.Request_Settings_builder{
		ModelConfig: warpapi.Request_Settings_ModelConfig_builder{
			Base:                        stringPtr(normalizeWarpModel(req.Model)),
			CliAgent:                    stringPtr(cliAgentModel),
			ComputerUseAgent:            stringPtr(computerAgentModel),
			BaseModelContextWindowLimit: &contextLimit,
		}.Build(),
		WebContextRetrievalEnabled:                 boolPtr(toolsEnabled),
		SupportsParallelToolCalls:                  boolPtr(parallelTools),
		UseAnthropicTextEditorTools:                boolPtr(false),
		PlanningEnabled:                            boolPtr(false),
		WarpDriveContextEnabled:                    boolPtr(false),
		SupportsCreateFiles:                        boolPtr(toolsEnabled),
		SupportedTools:                             supportedTools,
		SupportsLongRunningCommands:                boolPtr(toolsEnabled),
		ShouldPreserveFileContentInHistory:         boolPtr(true),
		SupportsTodosUi:                            boolPtr(false),
		SupportsLinkedCodeBlocks:                   boolPtr(false),
		SupportsStartedChildTaskMessage:            boolPtr(false),
		SupportsSuggestPrompt:                      boolPtr(false),
		SupportsReadImageFiles:                     boolPtr(false),
		SupportsReasoningMessage:                   boolPtr(true),
		AutonomyLevel:                              &autonomy,
		IsolationLevel:                             &isolation,
		WebSearchEnabled:                           boolPtr(toolsEnabled),
		SupportedCliAgentTools:                     supportedCliTools,
		SupportsV4AFileDiffs:                       boolPtr(false),
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

var officialSupportedTools = []warpapi.ToolType{
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

var warpTextOnlyToolFence = []warpapi.ToolType{warpapi.ToolType_SUGGEST_PROMPT}

var officialSupportedCliAgentTools = []warpapi.ToolType{
	warpapi.ToolType_WRITE_TO_LONG_RUNNING_SHELL_COMMAND,
	warpapi.ToolType_READ_SHELL_COMMAND_OUTPUT,
	warpapi.ToolType_GREP,
	warpapi.ToolType_FILE_GLOB,
	warpapi.ToolType_FILE_GLOB_V2,
	warpapi.ToolType_READ_FILES,
}

func buildMCPContext(tools []toolDef) (*warpapi.Request_MCPContext, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	mcpTools := make([]*warpapi.Request_MCPContext_MCPTool, 0, len(tools))
	for _, tool := range tools {
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
		Name:        stringPtr("client"),
		Description: stringPtr("Tools declared by the client request"),
		Id:          stringPtr("client-request-tools"),
		Tools:       mcpTools,
	}.Build()
	return warpapi.Request_MCPContext_builder{
		Servers: []*warpapi.Request_MCPContext_MCPServer{server},
	}.Build(), nil
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

		canonicalName := toolname.NormalizeToolNameFallback(name)
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
	m, _ := v.(map[string]interface{})
	return m
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
	cleaned := cleanWarpSchema(schema, true)
	if cleaned == nil {
		return nil
	}
	filtered := filterWarpSchemaProperties(name, cleaned)
	if filtered == nil {
		return nil
	}
	cleaned = filtered
	if util.SchemaJSONLen(cleaned) <= maxWarpToolSchemaJSONLen {
		return cleaned
	}
	stripped := cleanWarpSchema(cleaned, false)
	if util.SchemaJSONLen(stripped) <= maxWarpToolSchemaJSONLen {
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

func cleanWarpSchema(schema map[string]interface{}, keepDescriptions bool) map[string]interface{} {
	if schema == nil {
		return nil
	}
	sanitized := map[string]interface{}{}
	for _, key := range []string{"type", "description", "properties", "required", "enum", "items"} {
		if key == "description" && !keepDescriptions {
			continue
		}
		if v, ok := schema[key]; ok {
			sanitized[key] = v
		}
	}
	if props, ok := sanitized["properties"].(map[string]interface{}); ok {
		cleanProps := map[string]interface{}{}
		for name, prop := range props {
			cleanProps[name] = cleanWarpSchemaValue(prop, keepDescriptions)
		}
		sanitized["properties"] = cleanProps
	}
	if items, ok := sanitized["items"]; ok {
		sanitized["items"] = cleanWarpSchemaValue(items, keepDescriptions)
	}
	return sanitized
}

func cleanWarpSchemaValue(value interface{}, keepDescriptions bool) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		return cleanWarpSchema(v, keepDescriptions)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, cleanWarpSchemaValue(item, keepDescriptions))
		}
		return out
	default:
		return value
	}
}
