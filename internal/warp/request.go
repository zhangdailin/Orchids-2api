package warp

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/orchids"
	"orchids-api/internal/prompt"
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

func (e *encoder) writeBool(field int, value bool) {
	e.writeKey(field, 0)
	if value {
		e.writeVarint(1)
	} else {
		e.writeVarint(0)
	}
}

func (e *encoder) writeMessage(field int, msg []byte) {
	e.writeKey(field, 2)
	e.writeVarint(uint64(len(msg)))
	e.b = append(e.b, msg...)
}

func (e *encoder) writePackedVarints(field int, values []int) {
	if len(values) == 0 {
		return
	}
	inner := encoder{}
	for _, v := range values {
		inner.writeVarint(uint64(v))
	}
	e.writeKey(field, 2)
	e.writeVarint(uint64(len(inner.b)))
	e.b = append(e.b, inner.b...)
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

const (
	templateQueryLen        = 9  // "你好呀"
	templateInputContextLen = 67 // 模板里的 InputContext 长度
)

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
	var out []byte
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

	fullQuery, isNew := buildWarpQuery(userText, history, toolResults, disableWarpTools)
	if fullQuery == "" {
		return nil, fmt.Errorf("empty prompt")
	}

	// 对齐 Python：无历史且无工具结果时用模板构建
	if len(history) == 0 && len(toolResults) == 0 {
		reqBytes, err := buildRequestBytesFromTemplate(fullQuery, isNew, disableWarpTools)
		if err != nil {
			return nil, err
		}
		if len(mcpContext) > 0 {
			reqBytes = append(reqBytes, encodeBytesField(6, mcpContext)...)
		}
		return reqBytes, nil
	}

	inputContext := buildInputContext(workdir)
	userQuery := buildUserQuery(fullQuery, isNew, false)
	input := buildInputWithUserQuery(inputContext, userQuery)
	settings := buildSettings(model, disableWarpTools)

	req := encoder{}
	// task_context: empty message (field 1, length 0) => 0a 00
	req.writeMessage(1, []byte{})
	req.writeMessage(2, input)
	req.writeMessage(3, settings)
	if len(mcpContext) > 0 {
		req.writeMessage(6, mcpContext)
	}

	_ = conversationID // 对齐 Python：不使用 conversation_id
	return req.bytes(), nil
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
				key := toolResult.ToolCallID + "|" + toolResult.Content
				if key == "|" {
					key = ""
				}
				if key != "" {
					if _, ok := toolResultSeen[key]; ok {
						continue
					}
					toolResultSeen[key] = struct{}{}
				}
				if lastUserTextIdx == -1 || i > lastUserTextIdx || (i == lastUserTextIdx && !hasText) {
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
		return fmt.Sprintf("<tool_use name=\"%s\">\n%s\n</tool_use>", name, args)
	}
	return fmt.Sprintf("<tool_use id=\"%s\" name=\"%s\">\n%s\n</tool_use>", id, name, args)
}

func formatWarpToolResult(id, content string) string {
	content = strings.TrimSpace(content)
	if id == "" {
		return fmt.Sprintf("<tool_result>\n%s\n</tool_result>", content)
	}
	return fmt.Sprintf("<tool_result id=\"%s\">\n%s\n</tool_result>", id, content)
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
	var parts []string
	if disableWarpTools {
		parts = append(parts, noWarpToolsPrompt)
	}

	if len(toolResults) > 0 {
		parts = append(parts, formatWarpHistory(history)...)
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

	if len(history) > 0 {
		parts = append(parts, formatWarpHistory(history)...)
		if strings.TrimSpace(userText) != "" {
			parts = append(parts, "User: "+userText)
		}
		return strings.Join(parts, "\n\n"), true
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
			parts = append(parts, formatWarpToolResult(msg.ToolCallID, msg.Content))
		}
	}
	return parts
}

func buildRequestBytesFromTemplate(userText string, isNew bool, disableWarpTools bool) ([]byte, error) {
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

	userInputsStart := 4 + 2 + templateInputContextLen
	if userInputsStart >= len(template) || template[userInputsStart] != 0x32 {
		pos := findBytes(template, []byte{0x32, 0x13})
		if pos == -1 {
			return nil, fmt.Errorf("warp template: user_inputs not found")
		}
		userInputsStart = pos
	}

	contextPart := template[4:userInputsStart]
	newInputContent := append(append([]byte(nil), contextPart...), userInputsContent...)
	newInputMsg := append([]byte{0x12}, encodeVarint(len(newInputContent))...)
	newInputMsg = append(newInputMsg, newInputContent...)

	// settings 起点基于模板原始 Input 长度 (0x5a = 90)
	settingsStart := 2 + 2 + 90
	if settingsStart > len(template) {
		return nil, fmt.Errorf("warp template: invalid settings offset")
	}
	rest := template[settingsStart:]

	result := append([]byte{0x0a, 0x00}, newInputMsg...)
	result = append(result, rest...)

	if disableWarpTools {
		result = removeSupportedTools(result)
	}
	return result, nil
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

	origSettingsLen := int(data[settingsTagPos+1])
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
		if settingsTagPos+1 < len(result) {
			result[settingsTagPos+1] = byte(newLen)
		}
	}
	return result
}

func findBytes(haystack, needle []byte) int {
	if len(needle) == 0 {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

func encodeBytesField(field int, value []byte) []byte {
	e := encoder{}
	e.writeBytes(field, value)
	return e.bytes()
}

func buildMetadata(conversationID string) []byte {
	meta := encoder{}

	// WARNING: Setting conversation_id in metadata can cause empty responses in modern Warp API.
	// We rely on manual context history concatenation instead.
	// if conversationID != "" {
	// 	meta.writeString(1, conversationID)
	// }

	// logging map (field 2)
	// Entry 1: key="entrypoint", value (string_value)="USER_INITIATED"
	entry1 := encoder{}
	entry1.writeString(1, "entrypoint")

	val1 := encoder{}
	val1.writeString(3, "USER_INITIATED") // google.protobuf.Value.string_value
	entry1.writeMessage(2, val1.bytes())

	meta.writeMessage(2, entry1.bytes())

	// Entry 2: key="is_auto_resume_after_error", value (bool_value)=false
	entry2 := encoder{}
	entry2.writeString(1, "is_auto_resume_after_error")
	val2 := encoder{}
	val2.writeBool(4, false)
	entry2.writeMessage(2, val2.bytes())
	meta.writeMessage(2, entry2.bytes())

	// Entry 3: key="is_autodetected_user_query", value (bool_value)=true
	entry3 := encoder{}
	entry3.writeString(1, "is_autodetected_user_query")
	val3 := encoder{}
	val3.writeBool(4, true)
	entry3.writeMessage(2, val3.bytes())
	meta.writeMessage(2, entry3.bytes())

	return meta.bytes()
}

func buildInputContext(workdir string) []byte {
	pwd := strings.TrimSpace(workdir)
	if pwd == "" {
		pwd, _ = os.Getwd()
	}
	home, _ := os.UserHomeDir()
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

func buildUserQuery(prompt string, isNew bool, includeAttachments bool) []byte {
	msg := encoder{}
	msg.writeString(1, prompt)
	// referenced_attachments (field 2) - omitted if empty
	if includeAttachments {
		msg.writeBytes(3, []byte{})
	}
	msg.writeBool(4, isNew) // is_new_conversation
	return msg.bytes()
}

func buildInput(contextBytes, userQueryBytes []byte) []byte {
	// Modern API uses user_inputs (field 6)
	// UserInputs (field 6) -> repeated UserInput (field 1) -> oneof input { UserQuery (field 1) }

	userInput := encoder{}
	userInput.writeMessage(1, userQueryBytes) // UserInput.user_query (field 1)

	userInputs := encoder{}
	userInputs.writeMessage(1, userInput.bytes()) // repeated UserInputs.inputs (field 1)

	input := encoder{}
	input.writeMessage(1, contextBytes)
	input.writeMessage(6, userInputs.bytes()) // Input.user_inputs (field 6)
	return input.bytes()
}

func buildInputWithUserQuery(contextBytes, userQueryBytes []byte) []byte {
	// 对齐 Python：使用 Input.user_query (field 2)
	input := encoder{}
	input.writeMessage(1, contextBytes)
	input.writeMessage(2, userQueryBytes)
	return input.bytes()
}

func buildSettings(model string, disableWarpTools bool) []byte {
	modelName := normalizeModel(model)
	modelCfg := encoder{}
	modelCfg.writeString(1, modelName)

	settings := encoder{}
	settings.writeMessage(1, modelCfg.bytes())
	settings.writeBool(2, true)
	settings.writeBool(3, true)
	settings.writeBool(4, true)
	settings.writeBool(6, true)
	settings.writeBool(7, true)
	settings.writeBool(8, true)
	settings.writeBool(10, true)
	settings.writeBool(11, true)
	settings.writeBool(12, true)
	settings.writeBool(13, true)
	settings.writeBool(14, true)
	settings.writeBool(15, true)
	settings.writeBool(16, true)
	settings.writeBool(17, true)
	settings.writeBool(21, true)

	if !disableWarpTools {
		settings.writePackedVarints(9, []int{6, 7, 12, 8, 9, 15, 14, 0, 11, 16, 10, 20, 17, 19, 18, 2, 3, 1, 13})
	}
	settings.writePackedVarints(22, []int{10, 20, 6, 7, 12, 9, 2, 1})
	settings.writeBool(23, true)

	return settings.bytes()
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

func convertTools(tools []interface{}) []toolDef {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]toolDef, 0, len(tools))
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
				description, _ := fn["description"].(string)
				schema := schemaMap(fn["parameters"])
				if name != "" {
					defs = append(defs, toolDef{Name: name, Description: description, Schema: schema})
				}
				continue
			}
		}
		name, _ := m["name"].(string)
		if orchids.DefaultToolMapper.IsBlocked(name) {
			continue
		}
		name = orchids.NormalizeToolName(name)
		description, _ := m["description"].(string)
		schema := schemaMap(m["input_schema"])
		if schema == nil {
			schema = schemaMap(m["parameters"])
		}
		if name != "" {
			defs = append(defs, toolDef{Name: name, Description: description, Schema: schema})
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

func normalizeModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return defaultModel
	}

	known := map[string]struct{}{
		"auto":              {},
		"auto-efficient":    {},
		"auto-genius":       {},
		"warp-basic":        {},
		"gpt-5":             {},
		"gpt-4o":            {},
		"gpt-4.1":           {},
		"o3":                {},
		"o4-mini":           {},
		"gemini-2.5-pro":    {},
		"claude-4-sonnet":   {},
		"claude-4-opus":     {},
		"claude-4.1-opus":   {},
		"claude-4-5-sonnet": {},
		"claude-4-5-opus":   {},
	}
	if _, ok := known[model]; ok {
		return model
	}

	if strings.Contains(model, "sonnet-4-5") || strings.Contains(model, "sonnet 4.5") {
		return "claude-4-5-sonnet"
	}
	if strings.Contains(model, "opus-4-5") || strings.Contains(model, "opus 4.5") {
		return "claude-4-5-opus"
	}
	if strings.Contains(model, "sonnet-4") {
		return "claude-4-sonnet"
	}
	if strings.Contains(model, "opus-4") || strings.Contains(model, "opus 4") {
		return "claude-4-opus"
	}

	if model == "auto" {
		return "auto"
	}
	return "auto"
}
