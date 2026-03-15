package warp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

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

type promptBuild struct {
	Full            string
	Query           string
	BasePrompt      string
	HistoryText     string
	ToolResultsText string
}

type renderedBlock struct {
	role         string
	text         string
	isToolResult bool
}

func buildRequestBytes(req upstream.UpstreamRequest) (string, []byte, error) {
	built := buildPrompt(req.Prompt, req.Messages, req.System, req.Tools, req.NoTools, req.Workdir)
	if strings.TrimSpace(built.Full) == "" {
		return "", nil, fmt.Errorf("empty warp prompt")
	}

	disableWarpTools := req.NoTools || len(req.Tools) == 0
	payload, err := buildRequestBytesFromTemplate(
		built.Full,
		normalizeWarpTemplateModel(req.Model),
		isNewWarpConversation(req),
		disableWarpTools,
		req.Workdir,
	)
	if err != nil {
		return "", nil, err
	}

	if !disableWarpTools {
		mcpContext, err := buildMCPContext(req.Tools)
		if err != nil {
			return "", nil, err
		}
		if len(mcpContext) > 0 {
			payload = append(payload, encodeBytesField(6, mcpContext)...)
		}
	}

	return built.Full, payload, nil
}

func buildPrompt(promptText string, messages []prompt.Message, systemItems []prompt.SystemItem, _ []interface{}, _ bool, _ string) promptBuild {
	systemText := buildSystemText(systemItems, messages)
	rendered, queryText := renderConversation(messages, promptText)

	var conversation strings.Builder
	var historyText strings.Builder
	var toolResultsText strings.Builder
	for _, block := range rendered {
		section := renderConversationBlock(block)
		conversation.WriteString(section)
		if block.isToolResult {
			toolResultsText.WriteString(section)
		} else {
			historyText.WriteString(section)
		}
	}

	basePrompt := systemText + "<|conversation|>\n"

	var full strings.Builder
	full.WriteString(basePrompt)
	full.WriteString(conversation.String())
	full.WriteString("<|end_conversation|>\n")
	full.WriteString("<|assistant|>\n")

	return promptBuild{
		Full:            full.String(),
		Query:           queryText,
		BasePrompt:      basePrompt,
		HistoryText:     historyText.String(),
		ToolResultsText: toolResultsText.String(),
	}
}

func buildSystemText(systemItems []prompt.SystemItem, messages []prompt.Message) string {
	var custom []string
	for _, item := range systemItems {
		if text := sanitizeUTF8(strings.TrimSpace(item.Text)); text != "" {
			custom = append(custom, text)
		}
	}
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "system") {
			continue
		}
		if text := extractMessageText(msg.Content); text != "" {
			custom = append(custom, text)
		}
	}

	var b strings.Builder
	b.WriteString("<|system_prompt|>\n")
	b.WriteString("You are an AI assistant integrated into Warp terminal. ")
	b.WriteString("You help users with coding tasks, terminal commands, and software development. ")
	b.WriteString("Follow the user's instructions carefully and provide helpful, accurate responses.\n")
	b.WriteString("<|end_system_prompt|>\n")

	for _, text := range custom {
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteByte('\n')
		}
	}

	b.WriteString("<|agent_mode|>\n")
	b.WriteString("You have access to tools. Use them when needed.\n")
	b.WriteString("Format your responses clearly. Use markdown for code blocks. ")
	b.WriteString("When executing commands, show the command and explain the output. ")
	b.WriteString("Be concise but thorough.\n")
	b.WriteString("Do not execute destructive commands without confirmation. ")
	b.WriteString("Do not access or modify files outside the user's workspace. ")
	b.WriteString("Respect the user's privacy and do not share sensitive information.\n")

	return b.String()
}

func renderConversation(messages []prompt.Message, promptText string) ([]renderedBlock, string) {
	if len(messages) == 0 {
		promptText = strings.TrimSpace(promptText)
		if promptText == "" {
			return nil, ""
		}
		promptText = sanitizeUTF8(promptText)
		return []renderedBlock{{role: "user", text: promptText}}, promptText
	}

	var out []renderedBlock
	lastUserText := ""
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "" || role == "system" {
			continue
		}

		text, toolResults := renderMessageContent(msg.Content)
		if text != "" {
			out = append(out, renderedBlock{role: role, text: text})
			if role == "user" {
				lastUserText = text
			}
		}
		for _, result := range toolResults {
			out = append(out, renderedBlock{role: "tool", text: result, isToolResult: true})
		}
	}

	if len(out) == 0 && strings.TrimSpace(promptText) != "" {
		text := sanitizeUTF8(strings.TrimSpace(promptText))
		out = append(out, renderedBlock{role: "user", text: text})
		lastUserText = text
	}

	return out, lastUserText
}

func renderMessageContent(content prompt.MessageContent) (string, []string) {
	if content.IsString() {
		return sanitizeUTF8(strings.TrimSpace(content.GetText())), nil
	}

	var textParts []string
	var toolResults []string
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := sanitizeUTF8(strings.TrimSpace(block.Text)); text != "" {
				textParts = append(textParts, text)
			}
		case "tool_result":
			payload := stringifyValue(block.Content)
			if payload == "" {
				payload = "{}"
			}
			name := strings.TrimSpace(block.Name)
			if name != "" {
				toolResults = append(toolResults, fmt.Sprintf("<|tool_result:%s|>\n%s\n", name, payload))
			} else if block.ToolUseID != "" {
				toolResults = append(toolResults, fmt.Sprintf("<|tool_result:%s|>\n%s\n", block.ToolUseID, payload))
			} else {
				toolResults = append(toolResults, "<|tool_result|>\n"+payload+"\n")
			}
		}
	}

	return sanitizeUTF8(strings.TrimSpace(strings.Join(textParts, "\n"))), toolResults
}

func renderConversationBlock(block renderedBlock) string {
	switch block.role {
	case "assistant":
		return "<|assistant|>\n" + block.text + "\n"
	case "tool":
		return block.text
	default:
		return "<|user|>\n" + block.text + "\n"
	}
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



func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	buf = append(buf, byte(v))
	return buf
}

func isNewWarpConversation(req upstream.UpstreamRequest) bool {
	if strings.TrimSpace(req.ChatSessionID) != "" {
		return false
	}
	if len(req.Messages) == 0 {
		return true
	}
	meaningful := 0
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "" || role == "system" {
			continue
		}
		meaningful++
		if role == "assistant" {
			return false
		}
		if !msg.Content.IsString() {
			for _, block := range msg.Content.GetBlocks() {
				switch block.Type {
				case "tool_result", "tool_use":
					return false
				}
			}
		}
	}
	return meaningful <= 1
}

func EstimateInputTokens(promptText, _ string, messages []prompt.Message, _ []interface{}, _ bool) (InputTokenEstimate, error) {
	built := buildPrompt(promptText, messages, nil, nil, false, "")
	baseTokens := tiktoken.EstimateTextTokens(built.BasePrompt)
	historyTokens := tiktoken.EstimateTextTokens(built.HistoryText)
	toolResultTokens := tiktoken.EstimateTextTokens(built.ToolResultsText)
	queryTokens := tiktoken.EstimateTextTokens(built.Query)
	total := baseTokens + historyTokens + toolResultTokens

	return InputTokenEstimate{
		Profile:          "warp-codefreemax",
		QueryTokens:      queryTokens,
		BasePromptTokens: baseTokens,
		HistoryTokens:    historyTokens,
		ToolResultTokens: toolResultTokens,
		ToolSchemaTokens: 0,
		ToolCount:        0,
		Total:            total,
	}, nil
}

func ResolveModelAlias(model string) string {
	canonical := canonicalModelID(model)
	if canonical == "" {
		return ""
	}
	if _, ok := canonicalModelAliases[canonical]; ok {
		return canonical
	}
	return ""
}

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



func normalizeWarpTemplateModel(model string) string {
	canonical := canonicalModelID(model)
	if canonical == "" {
		return defaultModel
	}
	return canonical
}

func buildRequestBytesFromTemplate(userText, model string, isNew bool, disableWarpTools bool, workdir string) ([]byte, error) {
	template := append([]byte(nil), realRequestTemplate...)

	newQueryBytes := []byte(userText)
	userQueryContent := []byte{0x0a}
	userQueryContent = append(userQueryContent, appendVarint(nil, uint64(len(newQueryBytes)))...)
	userQueryContent = append(userQueryContent, newQueryBytes...)
	userQueryContent = append(userQueryContent, 0x1a, 0x00, 0x20)
	if isNew {
		userQueryContent = append(userQueryContent, 0x01)
	} else {
		userQueryContent = append(userQueryContent, 0x00)
	}

	userQueryTotalLen := len(userQueryContent)
	userInputContent := append([]byte{0x0a}, appendVarint(nil, uint64(userQueryTotalLen))...)
	userInputContent = append(userInputContent, userQueryContent...)

	inputsContent := append([]byte{0x0a}, appendVarint(nil, uint64(len(userInputContent)))...)
	inputsContent = append(inputsContent, userInputContent...)

	userInputsContent := append([]byte{0x32}, appendVarint(nil, uint64(len(inputsContent)))...)
	userInputsContent = append(userInputsContent, inputsContent...)

	contextEnc := encoder{}
	contextEnc.writeMessage(1, buildInputContext(workdir))
	contextPart := contextEnc.bytes()
	newInputContent := append(append([]byte(nil), contextPart...), userInputsContent...)
	newInputMsg := append([]byte{0x12}, appendVarint(nil, uint64(len(newInputContent)))...)
	newInputMsg = append(newInputMsg, newInputContent...)

	settingsStart := 2 + 2 + 90
	if settingsStart > len(template) {
		return nil, fmt.Errorf("warp template: invalid settings offset")
	}
	rest := template[settingsStart:]

	result := append([]byte{0x0a, 0x00}, newInputMsg...)
	result = append(result, rest...)
	result = patchTemplateModel(result, model)

	if disableWarpTools {
		result = removeSupportedTools(result)
	}
	return result, nil
}

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
		newVarint := appendVarint(nil, uint64(newLen))
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
				name = orchids.NormalizeToolNameFallback(name)
				if !isSupportedWarpTool(name) {
					continue
				}
				description, _ := fn["description"].(string)
				schema := compactWarpSchemaForTool(name, schemaMap(fn["parameters"]))
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

		name, _ := m["name"].(string)
		name = orchids.NormalizeToolNameFallback(name)
		if !isSupportedWarpTool(name) {
			continue
		}
		description, _ := m["description"].(string)
		schema := schemaMap(m["input_schema"])
		if schema == nil {
			schema = schemaMap(m["parameters"])
		}
		schema = compactWarpSchemaForTool(name, schema)
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


