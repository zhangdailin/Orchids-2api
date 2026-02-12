package warp

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
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
	reqBytes, err := buildRequestBytesFromTemplate(fullQuery, isNew, disableWarpTools, workdir)
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
	if strings.Contains(lower, "eoferror: eof when reading a line") {
		return true
	}
	return false
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
	content = strings.TrimSpace(content)
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
			parts = append(parts, formatWarpToolResult(msg.ToolCallID, msg.Content))
		}
	}
	return parts
}

func buildRequestBytesFromTemplate(userText string, isNew bool, disableWarpTools bool, workdir string) ([]byte, error) {
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
	maxWarpToolCount         = 24
	maxWarpToolDescLen       = 512
	maxWarpToolSchemaJSONLen = 4096
)

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
				description, _ := fn["description"].(string)
				schema := compactWarpSchema(schemaMap(fn["parameters"]))
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
		description, _ := m["description"].(string)
		schema := schemaMap(m["input_schema"])
		if schema == nil {
			schema = schemaMap(m["parameters"])
		}
		schema = compactWarpSchema(schema)
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
	runes := []rune(description)
	if len(runes) <= maxWarpToolDescLen {
		return description
	}
	return string(runes[:maxWarpToolDescLen]) + "...[truncated]"
}

func compactWarpSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	cleaned := cleanWarpSchema(schema)
	if cleaned == nil {
		return nil
	}
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
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return defaultModel
	}

	known := map[string]struct{}{
		"auto":                       {},
		"auto-efficient":             {},
		"auto-genius":                {},
		"warp-basic":                 {},
		"claude-4-sonnet":            {},
		"claude-4-5-sonnet":          {},
		"claude-4-5-sonnet-thinking": {},
		"claude-4-5-opus":            {},
		"claude-4-5-opus-thinking":   {},
		"claude-4-6-opus-high":       {},
		"claude-4-6-opus-max":        {},
		"claude-4-5-haiku":           {},
		"claude-4-opus":              {},
		"claude-4.1-opus":            {},
		"gpt-5":                      {},
		"gpt-5-low":                  {},
		"gpt-5-medium":               {},
		"gpt-5-high":                 {},
		"gpt-5-1-low":                {},
		"gpt-5-1-medium":             {},
		"gpt-5-1-high":               {},
		"gpt-5-1-codex-low":          {},
		"gpt-5-1-codex-medium":       {},
		"gpt-5-1-codex-high":         {},
		"gpt-5-1-codex-max-low":      {},
		"gpt-4o":                     {},
		"gpt-4.1":                    {},
		"o3":                         {},
		"o4-mini":                    {},
		"gemini-2-5-pro":             {},
		"gemini-2.5-pro":             {},
		"gemini-3-pro":               {},
	}
	if _, ok := known[model]; ok {
		return model
	}

	if strings.Contains(model, "sonnet-4-5") || strings.Contains(model, "sonnet 4.5") {
		if strings.Contains(model, "thinking") {
			return "claude-4-5-sonnet-thinking"
		}
		return "claude-4-5-sonnet"
	}
	if strings.Contains(model, "opus-4-6") || strings.Contains(model, "opus 4.6") {
		if strings.Contains(model, "max") {
			return "claude-4-6-opus-max"
		}
		return "claude-4-6-opus-high"
	}
	if strings.Contains(model, "opus-4-5") || strings.Contains(model, "opus 4.5") {
		if strings.Contains(model, "thinking") {
			return "claude-4-5-opus-thinking"
		}
		return "claude-4-5-opus"
	}
	if strings.Contains(model, "haiku-4-5") || strings.Contains(model, "haiku 4.5") {
		return "claude-4-5-haiku"
	}
	if strings.Contains(model, "sonnet-4") {
		return "claude-4-sonnet"
	}
	if strings.Contains(model, "opus-4") || strings.Contains(model, "opus 4") {
		return "claude-4-opus"
	}

	// Gemini 模糊匹配
	if strings.Contains(model, "gemini-3") || strings.Contains(model, "gemini 3") {
		return "gemini-3-pro"
	}
	if strings.Contains(model, "gemini-2-5") || strings.Contains(model, "gemini-2.5") || strings.Contains(model, "gemini 2.5") {
		return "gemini-2-5-pro"
	}

	// GPT-5.1 Codex Max 模糊匹配
	if strings.Contains(model, "gpt-5-1-codex-max") || strings.Contains(model, "gpt-5.1-codex-max") {
		return "gpt-5-1-codex-max-low"
	}

	// Haiku / Sonnet / Opus 通配
	if strings.Contains(model, "haiku") {
		return "claude-4-5-haiku"
	}
	if strings.Contains(model, "sonnet") {
		if strings.Contains(model, "thinking") {
			return "claude-4-5-sonnet-thinking"
		}
		return "claude-4-5-sonnet"
	}
	if strings.Contains(model, "opus") {
		if strings.Contains(model, "thinking") {
			return "claude-4-5-opus-thinking"
		}
		return "claude-4-5-opus"
	}

	return "auto"
}
