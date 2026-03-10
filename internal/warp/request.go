package warp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/goccy/go-json"
	"html"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/orchids"
	"orchids-api/internal/prompt"
	"orchids-api/internal/tiktoken"
)

type encoder struct {
	b []byte
}

func (e *encoder) bytes() []byte {
	return e.b
}

func (e *encoder) writeVarint(x uint64) {
	for x >= 0x80 {
		e.b = append(e.b, byte(x)|0x80)
		x >>= 7
	}
	e.b = append(e.b, byte(x))
}

func (e *encoder) writeKey(field int, wire int) {
	e.writeVarint(uint64(field<<3 | wire))
}

func (e *encoder) writeString(field int, value string) {
	e.writeKey(field, 2)
	e.writeVarint(uint64(len(value)))
	e.b = append(e.b, value...)
}

func (e *encoder) writeBytes(field int, value []byte) {
	e.writeKey(field, 2)
	e.writeVarint(uint64(len(value)))
	e.b = append(e.b, value...)
}

func (e *encoder) writeMessage(field int, msg []byte) {
	e.writeKey(field, 2)
	e.writeVarint(uint64(len(msg)))
	e.b = append(e.b, msg...)
}

type warpToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type warpHistoryMessage struct {
	Role       string
	Content    string
	ToolCalls  []warpToolCall
	ToolCallID string
}

type warpToolResult struct {
	ToolCallID string
	Content    string
}

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

var realRequestTemplate = mustDecodeHex("0a00125a0a430a1e0a0d2f55736572732f6c6f66796572120d2f55736572732f6c6f6679657212070a054d61634f531a0a0a037a73681203352e39220c08eeb8d3cb0610908ef0bd0232130a110a0f0a09e4bda0e5a5bde591801a0020011a660a210a0f636c617564652d342d352d6f707573220e636c692d6167656e742d6175746f1001180120013001380140014a1306070c08090f0e000b100a141113120203010d500158016001680170017801800101880101a80101b201070a1406070c0201b801012264121e0a0a656e747279706f696e7412101a0e555345525f494e4954494154454412200a1a69735f6175746f5f726573756d655f61667465725f6572726f721202200012200a1a69735f6175746f64657465637465645f757365725f717565727912022001")

var supportedToolsPattern = mustDecodeHex("4a1306070c08090f0e000b100a141113120203010d")
var clientSupportedToolsPattern = mustDecodeHex("b201070a1406070c0201")

func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("warp: invalid hex template: %v", err))
	}
	return b
}

func encodeVarint(value int) []byte {
	if value < 0 {
		return []byte{0}
	}
	x := uint64(value)
	out := make([]byte, 0, 10) // varint max 10 bytes
	for x >= 0x80 {
		out = append(out, byte(x)|0x80)
		x >>= 7
	}
	out = append(out, byte(x))
	return out
}

func buildRequestBytes(promptText, model string, messages []prompt.Message, mcpContext []byte, disableWarpTools bool, workdir, conversationID string) ([]byte, error) {
	userText, history, toolResults, err := extractWarpConversation(messages, promptText)
	if err != nil {
		return nil, err
	}
	normalizedModel := normalizeModel(model)

	fullQuery, isNew := buildWarpQuery(userText, history, toolResults, disableWarpTools)
	if fullQuery == "" {
		return nil, fmt.Errorf("empty prompt")
	}

	// 有真实上游 conversationID 时标记为非新对话，让上游延续会话。
	// 本地生成的随机 ID（chat_ 前缀）不应覆盖 isNew，否则首次请求会被错误标记为续接。
	isUpstreamConvID := conversationID != "" && !strings.HasPrefix(conversationID, "chat_")
	if isUpstreamConvID {
		isNew = false
	}

	// 统一使用模板路径构建请求。历史已由 buildWarpQuery 扁平化到 fullQuery 中。
	// 注意：在 task_context 中注入 conversationID 会触发 Warp 400
	// (invalid AIAgentRequest: cannot parse invalid wire-format data)。
	// 当前保持空 task_context，依赖历史拼接延续上下文。
	reqBytes, err := buildRequestBytesFromTemplate(fullQuery, normalizedModel, isNew, disableWarpTools, workdir)
	if err != nil {
		return nil, err
	}
	if len(mcpContext) > 0 {
		reqBytes = append(reqBytes, encodeBytesField(6, mcpContext)...)
	}
	return reqBytes, nil
}

type parsedWarpMessage struct {
	role        string
	text        string
	toolUses    []prompt.ContentBlock
	toolResults []prompt.ContentBlock
}

func extractWarpConversation(messages []prompt.Message, promptText string) (string, []warpHistoryMessage, []warpToolResult, error) {
	if len(messages) == 0 {
		promptText = strings.TrimSpace(stripWarpMetaTags(promptText))
		if promptText == "" {
			return "", nil, nil, fmt.Errorf("empty prompt")
		}
		return promptText, nil, nil, nil
	}

	parsed := make([]parsedWarpMessage, 0, len(messages))
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "system" {
			continue
		}
		text, toolUses, toolResults := splitWarpContent(msg.Content)
		text = strings.TrimSpace(stripWarpMetaTags(text))
		parsed = append(parsed, parsedWarpMessage{
			role:        role,
			text:        text,
			toolUses:    toolUses,
			toolResults: toolResults,
		})
	}

	lastUserTextIdx := -1
	for i, msg := range parsed {
		if msg.role == "user" && strings.TrimSpace(msg.text) != "" {
			lastUserTextIdx = i
		}
	}

	var (
		userText    string
		history     []warpHistoryMessage
		toolResults []warpToolResult
	)
	toolResultSeen := map[string]struct{}{}

	for i, msg := range parsed {
		switch msg.role {
		case "user":
			hasText := strings.TrimSpace(msg.text) != ""
			if hasText {
				if i == lastUserTextIdx {
					userText = msg.text
				} else {
					history = append(history, warpHistoryMessage{Role: "user", Content: msg.text})
				}
			}

			for _, block := range msg.toolResults {
				toolResult := warpToolResult{
					ToolCallID: block.ToolUseID,
					Content:    strings.TrimSpace(stripWarpMetaTags(stringifyWarpValue(block.Content))),
				}
				if isNoiseToolResult(toolResult.Content) {
					continue
				}
				key := toolResultDedupKey(toolResult)
				if key != "" {
					if _, ok := toolResultSeen[key]; ok {
						continue
					}
					toolResultSeen[key] = struct{}{}
				}
				if lastUserTextIdx == -1 || i >= lastUserTextIdx {
					toolResults = append(toolResults, toolResult)
				} else {
					history = append(history, warpHistoryMessage{
						Role:       "tool",
						Content:    toolResult.Content,
						ToolCallID: toolResult.ToolCallID,
					})
				}
			}

		case "assistant":
			if lastUserTextIdx == -1 || i < lastUserTextIdx {
				toolCalls := convertWarpToolCalls(msg.toolUses)
				content := strings.TrimSpace(stripWarpMetaTags(msg.text))
				if content != "" || len(toolCalls) > 0 {
					history = append(history, warpHistoryMessage{
						Role:      "assistant",
						Content:   content,
						ToolCalls: toolCalls,
					})
				}
			}
		}
	}

	if userText == "" && len(toolResults) == 0 {
		return "", nil, nil, fmt.Errorf("no user message or tool results found")
	}
	return userText, history, toolResults, nil
}

// stripWarpMetaTags 清理系统提示残留（system-reminder/IDE 上下文）
func stripWarpMetaTags(text string) string {
	if text == "" {
		return text
	}
	out := stripTagBlocks(text, "system-reminder")
	out = stripTagBlocks(out, "ide_opened_file")
	out = stripTagBlocks(out, "ide_selection")
	out = filterWarpLogLines(out)
	return strings.TrimSpace(out)
}

func stripTagBlocks(text, tag string) string {
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"
	if !strings.Contains(text, startTag) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	i := 0
	for i < len(text) {
		start := strings.Index(text[i:], startTag)
		if start == -1 {
			sb.WriteString(text[i:])
			break
		}
		sb.WriteString(text[i : i+start])
		endStart := i + start + len(startTag)
		end := strings.Index(text[endStart:], endTag)
		if end == -1 {
			// No closing tag found — preserve the remaining text as-is
			sb.WriteString(text[i+start:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}

func filterWarpLogLines(text string) string {
	if text == "" {
		return text
	}
	if !strings.Contains(text, "[System:") && !strings.Contains(text, "[Scanning:") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[System:") || strings.HasPrefix(trimmed, "[Scanning:") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isNoiseToolResult(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	if strings.Contains(lower, "unsupported tool") {
		return true
	}
	if strings.Contains(lower, "unsupported tool:") {
		return true
	}
	if strings.Contains(lower, "<tool_use_error>") || strings.Contains(lower, "no such tool available") {
		return true
	}
	if isBenignNoopShellError(lower) {
		return true
	}
	return false
}

func toolResultDedupKey(result warpToolResult) string {
	content := strings.TrimSpace(result.Content)
	id := strings.TrimSpace(result.ToolCallID)
	if content == "" && id == "" {
		return ""
	}
	if shouldDedupToolResultByContent(content) {
		return "content:" + content
	}
	return id + "|" + content
}

func shouldDedupToolResultByContent(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, "eoferror: eof when reading a line")
}

func isBenignNoopShellError(lower string) bool {
	if strings.Contains(lower, "no matches found:") && strings.Contains(lower, "*") {
		return true
	}
	if strings.Contains(lower, "rm:") && strings.Contains(lower, "no such file or directory") {
		return true
	}
	if strings.Contains(lower, "cannot remove") && strings.Contains(lower, "no such file or directory") {
		return true
	}
	return false
}

func splitWarpContent(content prompt.MessageContent) (string, []prompt.ContentBlock, []prompt.ContentBlock) {
	if content.IsString() {
		return content.GetText(), nil, nil
	}

	var (
		textParts   []string
		toolUses    []prompt.ContentBlock
		toolResults []prompt.ContentBlock
	)
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			toolUses = append(toolUses, block)
		case "tool_result":
			toolResults = append(toolResults, block)
		}
	}
	return strings.Join(textParts, ""), toolUses, toolResults
}

func convertWarpToolCalls(blocks []prompt.ContentBlock) []warpToolCall {
	if len(blocks) == 0 {
		return nil
	}
	toolCalls := make([]warpToolCall, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		args := stringifyWarpValue(block.Input)
		toolCalls = append(toolCalls, warpToolCall{
			ID:        block.ID,
			Name:      block.Name,
			Arguments: args,
		})
	}
	return toolCalls
}

func formatWarpToolUse(call warpToolCall) string {
	name := strings.TrimSpace(call.Name)
	if name == "" {
		return ""
	}
	args := strings.TrimSpace(call.Arguments)
	if args == "" {
		args = "{}"
	}
	id := strings.TrimSpace(call.ID)
	if id == "" {
		return fmt.Sprintf("<tool_use name=\"%s\">\n%s\n</tool_use>", html.EscapeString(name), args)
	}
	return fmt.Sprintf("<tool_use id=\"%s\" name=\"%s\">\n%s\n</tool_use>", html.EscapeString(id), html.EscapeString(name), args)
}

func formatWarpToolResult(id, content string) string {
	return formatWarpToolResultWithMode(id, content, false)
}

func formatWarpToolResultWithMode(id, content string, historyMode bool) string {
	content = compactWarpToolResultContent(content, historyMode)
	if id == "" {
		return fmt.Sprintf("<tool_result>\n%s\n</tool_result>", content)
	}
	return fmt.Sprintf("<tool_result id=\"%s\">\n%s\n</tool_result>", html.EscapeString(id), content)
}

func stringifyWarpValue(value interface{}) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

func buildWarpQuery(userText string, history []warpHistoryMessage, toolResults []warpToolResult, disableWarpTools bool) (string, bool) {
	parts := []string{singleResultPrompt}

	if len(toolResults) > 0 {
		if disableWarpTools {
			parts = append(parts, noWarpToolsPrompt)
		}
		parts = append(parts, formatWarpFollowupHistory(history)...)
		for _, tr := range toolResults {
			parts = append(parts, formatWarpToolResult(tr.ToolCallID, tr.Content))
		}
		if strings.TrimSpace(userText) != "" {
			parts = append(parts, "User: "+userText)
		} else {
			parts = append(parts, "User: Please analyze the tool results above and provide your response.")
		}
		return strings.Join(parts, "\n\n"), false
	}

	if disableWarpTools {
		parts = append(parts, noWarpToolsPrompt)
	}

	if len(history) > 0 {
		parts = append(parts, formatWarpHistory(history)...)
		if strings.TrimSpace(userText) != "" {
			parts = append(parts, "User: "+userText)
		}
		return strings.Join(parts, "\n\n"), false // 有历史时不是新对话
	}

	if strings.TrimSpace(userText) == "" {
		return "", true
	}
	if len(parts) > 0 {
		parts = append(parts, userText)
		return strings.Join(parts, "\n\n"), true
	}
	return userText, true
}

func formatWarpHistory(history []warpHistoryMessage) []string {
	if len(history) == 0 {
		return nil
	}
	parts := make([]string, 0, len(history))
	for _, msg := range history {
		switch msg.Role {
		case "user":
			parts = append(parts, "User: "+msg.Content)
		case "assistant":
			if msg.Content != "" {
				parts = append(parts, "Assistant: "+msg.Content)
			}
			for _, tc := range msg.ToolCalls {
				if formatted := formatWarpToolUse(tc); formatted != "" {
					parts = append(parts, formatted)
				}
			}
		case "tool":
			parts = append(parts, formatWarpToolResultWithMode(msg.ToolCallID, msg.Content, true))
		}
	}
	return parts
}

func formatWarpFollowupHistory(history []warpHistoryMessage) []string {
	if len(history) == 0 {
		return nil
	}
	parts := make([]string, 0, len(history))
	for _, msg := range history {
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				parts = append(parts, "User: "+msg.Content)
			}
		case "assistant":
			if msg.Content != "" {
				parts = append(parts, "Assistant: "+msg.Content)
			}
		}
	}
	return parts
}

func EstimateInputTokens(promptText, model string, messages []prompt.Message, tools []interface{}, disableWarpTools bool) (InputTokenEstimate, error) {
	userText, history, toolResults, err := extractWarpConversation(messages, promptText)
	if err != nil {
		return InputTokenEstimate{}, err
	}

	fullQuery, _ := buildWarpQuery(userText, history, toolResults, disableWarpTools)
	if strings.TrimSpace(fullQuery) == "" {
		return InputTokenEstimate{}, fmt.Errorf("empty warp query")
	}

	defs := convertTools(tools)
	toolSchemaTokens := estimateWarpToolSchemaTokens(defs)
	historyParts := formatWarpHistory(history)
	if len(toolResults) > 0 {
		historyParts = formatWarpFollowupHistory(history)
	}
	historyTokens := estimateWarpTextTokens(historyParts)
	toolResultTokens := estimateWarpToolResultTokens(toolResults)
	queryTokens := tiktoken.EstimateTextTokens(fullQuery)
	baseTokens := queryTokens - historyTokens - toolResultTokens
	if baseTokens < 0 {
		baseTokens = queryTokens
	}

	return InputTokenEstimate{
		Profile:          classifyWarpProfile(history, toolResults, disableWarpTools, len(defs)),
		QueryTokens:      queryTokens,
		BasePromptTokens: baseTokens,
		HistoryTokens:    historyTokens,
		ToolResultTokens: toolResultTokens,
		ToolSchemaTokens: toolSchemaTokens,
		ToolCount:        len(defs),
		Total:            queryTokens + toolSchemaTokens,
	}, nil
}

func classifyWarpProfile(history []warpHistoryMessage, toolResults []warpToolResult, disableWarpTools bool, toolCount int) string {
	switch {
	case len(toolResults) > 0:
		return "warp-tool-result"
	case len(history) > 0:
		return "warp-history"
	case toolCount > 0 && !disableWarpTools:
		return "warp-tools"
	case disableWarpTools:
		return "warp-no-tools"
	default:
		return "warp"
	}
}

func estimateWarpTextTokens(parts []string) int {
	if len(parts) == 0 {
		return 0
	}
	return tiktoken.EstimateTextTokens(strings.Join(parts, "\n\n"))
}

func estimateWarpToolResultTokens(results []warpToolResult) int {
	if len(results) == 0 {
		return 0
	}
	parts := make([]string, 0, len(results))
	for _, tr := range results {
		parts = append(parts, formatWarpToolResult(tr.ToolCallID, tr.Content))
	}
	return estimateWarpTextTokens(parts)
}

func estimateWarpToolSchemaTokens(defs []toolDef) int {
	if len(defs) == 0 {
		return 0
	}
	parts := make([]string, 0, len(defs))
	for _, def := range defs {
		var sb strings.Builder
		sb.WriteString(def.Name)
		if desc := strings.TrimSpace(def.Description); desc != "" {
			sb.WriteString("\n")
			sb.WriteString(desc)
		}
		if len(def.Schema) > 0 {
			if raw, err := json.Marshal(def.Schema); err == nil && len(raw) > 0 {
				sb.WriteString("\n")
				sb.Write(raw)
			}
		}
		parts = append(parts, sb.String())
	}
	return estimateWarpTextTokens(parts)
}

func buildRequestBytesFromTemplate(userText, model string, isNew bool, disableWarpTools bool, workdir string) ([]byte, error) {
	template := append([]byte(nil), realRequestTemplate...)

	newQueryBytes := []byte(userText)
	userQueryContent := []byte{0x0a}
	userQueryContent = append(userQueryContent, encodeVarint(len(newQueryBytes))...)
	userQueryContent = append(userQueryContent, newQueryBytes...)
	userQueryContent = append(userQueryContent, 0x1a, 0x00, 0x20)
	if isNew {
		userQueryContent = append(userQueryContent, 0x01)
	} else {
		userQueryContent = append(userQueryContent, 0x00)
	}

	userQueryTotalLen := len(userQueryContent)
	userInputContent := append([]byte{0x0a}, encodeVarint(userQueryTotalLen)...)
	userInputContent = append(userInputContent, userQueryContent...)

	inputsContent := append([]byte{0x0a}, encodeVarint(len(userInputContent))...)
	inputsContent = append(inputsContent, userInputContent...)

	userInputsContent := append([]byte{0x32}, encodeVarint(len(inputsContent))...)
	userInputsContent = append(userInputsContent, inputsContent...)

	contextEnc := encoder{}
	contextEnc.writeMessage(1, buildInputContext(workdir))
	contextPart := contextEnc.bytes()
	newInputContent := append(append([]byte(nil), contextPart...), userInputsContent...)
	newInputMsg := append([]byte{0x12}, encodeVarint(len(newInputContent))...)
	newInputMsg = append(newInputMsg, newInputContent...)

	// settings 起点基于模板原始 Input 长度 (0x5a = 90)
	settingsStart := 2 + 2 + 90
	if settingsStart > len(template) {
		return nil, fmt.Errorf("warp template: invalid settings offset")
	}
	rest := template[settingsStart:]

	// 模板开头保留空 task_context: 0a 00
	result := append([]byte{0x0a, 0x00}, newInputMsg...)
	result = append(result, rest...)
	result = patchTemplateModel(result, model)

	if disableWarpTools {
		result = removeSupportedTools(result)
	}
	return result, nil
}

// patchTemplateModel rewrites the model entry in the static Warp protobuf template.
// The template currently stores one model slot as:
// 0a <entry_len> 0a <model_len> <model> 22 <id_len> <identifier>
// and is wrapped by a settings field:
// 1a <settings_len> ...
// We patch model bytes and adjust the two enclosing single-byte lengths.
func patchTemplateModel(data []byte, model string) []byte {
	target := strings.TrimSpace(model)
	if target == "" {
		target = defaultModel
	}
	modelBytes := []byte(target)
	if len(modelBytes) == 0 || len(modelBytes) > 0x7f {
		return data
	}

	idBytes := []byte(identifier)
	idPos := findBytes(data, idBytes)
	if idPos < 2 || data[idPos-2] != 0x22 {
		return data
	}
	idLen := int(data[idPos-1])
	if idLen != len(idBytes) {
		return data
	}

	modelTagPos := -1
	oldModelLen := 0
	for i := idPos - 3; i >= 0 && i >= idPos-96; i-- {
		if data[i] != 0x0a || i+1 >= len(data) {
			continue
		}
		l := int(data[i+1])
		if i+2+l == idPos-2 {
			modelTagPos = i
			oldModelLen = l
			break
		}
	}
	if modelTagPos < 0 {
		return data
	}

	entryTagPos := modelTagPos - 2
	if entryTagPos < 0 || data[entryTagPos] != 0x0a {
		return data
	}
	settingsTagPos := entryTagPos - 2
	if settingsTagPos < 0 || data[settingsTagPos] != 0x1a {
		return data
	}

	oldEntryLen := int(data[entryTagPos+1])
	expectedEntryLen := idPos + idLen - modelTagPos
	if oldEntryLen != expectedEntryLen {
		return data
	}

	oldSettingsLen := int(data[settingsTagPos+1])
	delta := len(modelBytes) - oldModelLen
	newEntryLen := oldEntryLen + delta
	newSettingsLen := oldSettingsLen + delta
	if newEntryLen < 0 || newEntryLen > 0x7f || newSettingsLen < 0 || newSettingsLen > 0x7f {
		return data
	}

	modelStart := modelTagPos + 2
	modelEnd := modelStart + oldModelLen
	if modelEnd > len(data) {
		return data
	}

	out := make([]byte, 0, len(data)+delta)
	out = append(out, data[:modelStart]...)
	out = append(out, modelBytes...)
	out = append(out, data[modelEnd:]...)

	out[modelTagPos+1] = byte(len(modelBytes))
	out[entryTagPos+1] = byte(newEntryLen)
	out[settingsTagPos+1] = byte(newSettingsLen)
	return out
}

func removeSupportedTools(data []byte) []byte {
	result := append([]byte(nil), data...)
	totalRemoved := 0

	settingsTagPos := -1
	for i := 0; i < len(data)-1; i++ {
		if data[i] == 0x1a && i > 50 {
			if i+3 < len(data) && data[i+2] == 0x0a && data[i+3] == 0x21 {
				settingsTagPos = i
				break
			}
		}
	}
	if settingsTagPos == -1 {
		return result
	}

	// Decode the settings length as a proper varint (may be >1 byte for lengths ≥128)
	origSettingsLen, varintLen := decodeVarintAt(data, settingsTagPos+1)
	if varintLen == 0 {
		return result
	}

	if pos := findBytes(result, supportedToolsPattern); pos != -1 {
		result = append(result[:pos], result[pos+len(supportedToolsPattern):]...)
		totalRemoved += len(supportedToolsPattern)
	}
	if pos := findBytes(result, clientSupportedToolsPattern); pos != -1 {
		result = append(result[:pos], result[pos+len(clientSupportedToolsPattern):]...)
		totalRemoved += len(clientSupportedToolsPattern)
	}

	if totalRemoved > 0 {
		newLen := origSettingsLen - totalRemoved
		if newLen < 0 {
			newLen = 0
		}
		newVarint := encodeVarint(newLen)
		// Replace the old varint with the new one; lengths may differ
		insertPos := settingsTagPos + 1
		oldEnd := insertPos + varintLen
		if oldEnd > len(result) {
			oldEnd = len(result)
		}
		tail := append([]byte(nil), result[oldEnd:]...)
		result = append(result[:insertPos], newVarint...)
		result = append(result, tail...)
	}
	return result
}

// decodeVarintAt reads a varint starting at data[offset] and returns (value, bytesConsumed).
// Returns (0, 0) if the varint cannot be read.
func decodeVarintAt(data []byte, offset int) (int, int) {
	var x uint64
	var s uint
	for i := offset; i < len(data); i++ {
		b := data[i]
		x |= uint64(b&0x7f) << s
		s += 7
		if b < 0x80 {
			return int(x), i - offset + 1
		}
		if s > 63 {
			return 0, 0
		}
	}
	return 0, 0
}

func findBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return -1
	}
	return bytes.Index(haystack, needle)
}

func encodeBytesField(field int, value []byte) []byte {
	e := encoder{}
	e.writeBytes(field, value)
	return e.bytes()
}

func buildInputContext(workdir string) []byte {
	pwd := strings.TrimSpace(workdir)
	home := ""
	shellName := "zsh"
	shellVersion := "5.9"

	dir := encoder{}
	dir.writeString(1, pwd)
	dir.writeString(2, home)

	osCtx := encoder{}
	osCtx.writeString(1, "MacOS")
	osCtx.writeString(2, "")

	shell := encoder{}
	shell.writeString(1, shellName)
	shell.writeString(2, shellVersion)

	ts := encoder{}
	now := time.Now()
	ts.writeKey(1, 0)
	ts.writeVarint(uint64(now.Unix()))
	ts.writeKey(2, 0)
	ts.writeVarint(uint64(now.Nanosecond()))

	ctx := encoder{}
	ctx.writeMessage(1, dir.bytes())
	ctx.writeMessage(2, osCtx.bytes())
	ctx.writeMessage(3, shell.bytes())
	ctx.writeMessage(4, ts.bytes())

	return ctx.bytes()
}

func buildMCPContext(tools []interface{}) ([]byte, error) {
	converted := convertTools(tools)
	if len(converted) == 0 {
		return nil, nil
	}
	ctx := encoder{}
	for _, tool := range converted {
		toolMsg := encoder{}
		toolMsg.writeString(1, tool.Name)
		toolMsg.writeString(2, tool.Description)
		if len(tool.Schema) > 0 {
			st, err := structpb.NewStruct(tool.Schema)
			if err != nil {
				return nil, err
			}
			encoded, err := proto.Marshal(st)
			if err != nil {
				return nil, err
			}
			toolMsg.writeMessage(3, encoded)
		}
		ctx.writeMessage(2, toolMsg.bytes())
	}

	return ctx.bytes(), nil
}

type toolDef struct {
	Name        string
	Description string
	Schema      map[string]interface{}
}

const (
	maxWarpToolCount         = 8
	maxWarpToolDescLen       = 128
	maxWarpToolSchemaJSONLen = 1024
)

var supportedWarpTools = map[string]struct{}{
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

func isSupportedWarpTool(name string) bool {
	_, ok := supportedWarpTools[name]
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
		if typ, _ := m["type"].(string); typ == "function" {
			if fn, ok := m["function"].(map[string]interface{}); ok {
				name, _ := fn["name"].(string)
				if orchids.DefaultToolMapper.IsBlocked(name) {
					continue
				}
				name = orchids.NormalizeToolName(name)
				if !isSupportedWarpTool(name) {
					continue
				}
				description, _ := fn["description"].(string)
				schema := compactWarpSchemaForTool(name, schemaMap(fn["parameters"]))
				if name != "" {
					key := strings.ToLower(strings.TrimSpace(name))
					if key == "" {
						continue
					}
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					defs = append(defs, toolDef{Name: name, Description: compactWarpDescription(description), Schema: schema})
					if len(defs) >= maxWarpToolCount {
						break
					}
				}
				continue
			}
		}
		name, _ := m["name"].(string)
		if orchids.DefaultToolMapper.IsBlocked(name) {
			continue
		}
		name = orchids.NormalizeToolName(name)
		if !isSupportedWarpTool(name) {
			continue
		}
		description, _ := m["description"].(string)
		schema := schemaMap(m["input_schema"])
		if schema == nil {
			schema = schemaMap(m["parameters"])
		}
		schema = compactWarpSchemaForTool(name, schema)
		if name != "" {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			defs = append(defs, toolDef{Name: name, Description: compactWarpDescription(description), Schema: schema})
			if len(defs) >= maxWarpToolCount {
				break
			}
		}
	}
	return defs
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
			if raw, ok := value.([]interface{}); ok {
				req := make([]interface{}, 0, len(raw))
				for _, item := range raw {
					name, _ := item.(string)
					if _, keep := allowed[name]; keep {
						req = append(req, item)
					}
				}
				if len(req) > 0 {
					out[key] = req
				}
			} else {
				out[key] = value
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

func normalizeModel(model string) string {
	normalized := normalizeWarpModelKey(model)
	if normalized == "" {
		return defaultModel
	}
	if mapped, ok := warpModelMap[normalized]; ok {
		return mapped
	}
	return defaultModel
}

// ResolveModelAlias maps a user-facing model ID to a Warp upstream model ID.
// Returns empty string if no known mapping exists.
func ResolveModelAlias(model string) string {
	normalized := normalizeWarpModelKey(model)
	if normalized == "" {
		return ""
	}
	if mapped, ok := warpModelMap[normalized]; ok {
		return mapped
	}
	return ""
}

var modelKeyReplacements = [][2]string{
	{"4.6", "4-6"}, {"4.5", "4-5"}, {"2.5", "2-5"},
	{"5.1", "5-1"}, {"5.2", "5-2"},
}

func normalizeWarpModelKey(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return ""
	}
	for _, r := range modelKeyReplacements {
		normalized = strings.ReplaceAll(normalized, r[0], r[1])
	}
	return normalized
}

var warpModelMap = map[string]string{
	"claude-4-sonnet":            "claude-4-sonnet",
	"claude-4-5-sonnet":          "claude-4-5-sonnet",
	"claude-4-5-sonnet-thinking": "claude-4-5-sonnet-thinking",
	"claude-4-6-sonnet":          "claude-4-6-sonnet-high",
	"claude-sonnet-4-6":          "claude-4-6-sonnet-high",
	"claude-4-6-opus-high":       "claude-4-6-opus-high",
	"claude-4-6-opus-max":        "claude-4-6-opus-max",
	"claude-4-6-sonnet-high":     "claude-4-6-sonnet-high",
	"claude-4-6-sonnet-max":      "claude-4-6-sonnet-max",
	"claude-4-6-opus":            "claude-4-6-opus-high",
	"claude-opus-4-6":            "claude-4-6-opus-high",
	"claude-4-5-opus-thinking":   "claude-4-5-opus-thinking",
	"gemini-2-5-pro":             "gemini-2-5-pro",
	"gemini-3-pro":               "gemini-3-pro",
	"claude-4-5-haiku":           "claude-4-5-haiku",
	"claude-4-5-opus":            "claude-4-5-opus",
	"gpt-5-1-codex-low":          "gpt-5-1-codex-low",
	"gpt-5-1-codex-medium":       "gpt-5-1-codex-medium",
	"gpt-5-1-codex-high":         "gpt-5-1-codex-high",
	"gpt-5-1-codex-max-low":      "gpt-5-1-codex-max-low",
	"gpt-5-1-codex-max-medium":   "gpt-5-1-codex-max-medium",
	"gpt-5-1-codex-max-high":     "gpt-5-1-codex-max-high",
	"gpt-5-1-codex-max-xhigh":    "gpt-5-1-codex-max-xhigh",
	"gpt-5-2-codex-low":          "gpt-5-2-codex-low",
}
