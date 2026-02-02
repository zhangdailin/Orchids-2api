package warp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

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

func buildRequestBytes(promptText, model string, messages []prompt.Message, mcpContext []byte, disableWarpTools bool, workdir, conversationID string) ([]byte, error) {
	promptText = strings.TrimSpace(promptText)
	if promptText == "" && len(messages) == 0 {
		return nil, fmt.Errorf("empty prompt")
	}

	// Format history using the project's standard formatter
	fullQuery := promptText
	if len(messages) > 0 {
		historyText := prompt.FormatMessagesAsMarkdown(messages, "")
		if historyText != "" {
			if promptText != "" {
				fullQuery = historyText + "\n\nUser: " + promptText
			} else {
				fullQuery = historyText
			}
		}
	}

	inputContext := buildInputContext(workdir)
	userQuery := buildUserQuery(fullQuery, len(messages) <= 1)
	input := buildInput(inputContext, userQuery)
	settings := buildSettings(model, disableWarpTools)

	req := encoder{}
	// task_context: empty message (field 1, length 0) => 0a 00
	req.writeMessage(1, []byte{})
	req.writeMessage(2, input)
	req.writeMessage(3, settings)
	
	// Metadata (field 4)
	metadata := buildMetadata(conversationID)
	req.writeMessage(4, metadata)

	if len(mcpContext) > 0 {
		req.writeMessage(6, mcpContext)
	}

	return req.bytes(), nil
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
	shellName := filepath.Base(os.Getenv("SHELL"))
	if shellName == "" {
		shellName = "zsh"
	}

	dir := encoder{}
	dir.writeString(1, pwd)
	dir.writeString(2, home)

	osCtx := encoder{}
	osCtx.writeString(1, osName)
	osCtx.writeString(2, "")

	shell := encoder{}
	shell.writeString(1, shellName)
	shell.writeString(2, "")

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

func buildUserQuery(prompt string, isNew bool) []byte {
	msg := encoder{}
	msg.writeString(1, prompt)
	// referenced_attachments (field 2) - omitted if empty
	// attachments_bytes (field 3) - omitted if empty
	msg.writeBool(4, isNew)      // is_new_conversation
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

func buildSettings(model string, disableWarpTools bool) []byte {
	modelName := normalizeModel(model)
	modelCfg := encoder{}
	modelCfg.writeString(1, modelName)
	modelCfg.writeString(2, "o3") // Standard planning model
	modelCfg.writeString(4, identifier)

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
	settings.writeBool(23, true)

	if !disableWarpTools {
		settings.writePackedVarints(9, []int{6, 7, 12, 8, 9, 15, 14, 0, 11, 16, 10, 20, 17, 19, 18, 2, 3, 1, 13})
	}
	settings.writePackedVarints(22, []int{10, 20, 6, 7, 12, 9, 2, 1})

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
				description, _ := fn["description"].(string)
				schema := schemaMap(fn["parameters"])
				if name != "" {
					defs = append(defs, toolDef{Name: name, Description: description, Schema: schema})
				}
				continue
			}
		}
		name, _ := m["name"].(string)
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
	return defaultModel
}
