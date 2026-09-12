package workbuddy

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// ChatMessage is one OpenAI-shaped message in the WorkBuddy payload.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the OpenAI tool-call shape used both inbound and outbound.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// buildMessages renders the request history. WorkBuddy validates roles against
// a whitelist and requires messages[0] to be a system message, so system items
// are emitted first, tool results become `tool` messages, and a minimal system
// prompt is prepended when the caller supplied none.
func buildMessages(req upstream.UpstreamRequest) []ChatMessage {
	out := make([]ChatMessage, 0, len(req.Messages)+len(req.System)+2)

	for _, item := range req.System {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			continue
		}
		out = append(out, ChatMessage{Role: "system", Content: item.Text})
	}

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "", "user":
			role = "user"
		case "developer":
			// `developer` is not in the upstream role whitelist; it is the new
			// name for the system role, so the rewrite is lossless.
			role = "system"
		}

		if msg.Content.IsString() {
			text := msg.Content.GetText()
			if strings.TrimSpace(text) == "" {
				// An empty text message carries nothing for the upstream and
				// trips its role/content validation.
				continue
			}
			out = append(out, ChatMessage{Role: role, Content: text, ReasoningContent: reasoningForReplay(msg)})
			continue
		}

		switch role {
		case "assistant":
			if converted, ok := convertAssistantMessage(msg); ok {
				out = append(out, converted)
			}
		default:
			out = append(out, convertBlockMessage(role, msg)...)
		}
	}

	if len(out) == 0 {
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			prompt = defaultSystem
		}
		out = append(out, ChatMessage{Role: "user", Content: prompt})
	}
	if !strings.EqualFold(strings.TrimSpace(out[0].Role), "system") {
		out = append([]ChatMessage{{Role: "system", Content: defaultSystem}}, out...)
	}
	return out
}

func reasoningForReplay(msg prompt.Message) string {
	if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
		return strings.TrimSpace(msg.ReasoningContent)
	}
	return ""
}

// convertAssistantMessage maps text, thinking and tool_use blocks onto one
// assistant message.
func convertAssistantMessage(msg prompt.Message) (ChatMessage, bool) {
	message := ChatMessage{Role: "assistant", ReasoningContent: strings.TrimSpace(msg.ReasoningContent)}
	var text []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case "thinking":
			if message.ReasoningContent == "" {
				message.ReasoningContent = strings.TrimSpace(block.Thinking)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = NewToolCallID()
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{
				ID:   id,
				Type: "function",
				Function: ToolCallFunction{
					Name:      name,
					Arguments: compactToolInput(block.Input),
				},
			})
		}
	}
	message.Content = strings.Join(text, "\n")
	return message, message.Content != "" || len(message.ToolCalls) > 0 || message.ReasoningContent != ""
}

// convertBlockMessage maps user/system blocks, splitting tool_result blocks
// into standalone `tool` messages so the assistant/tool pairing stays intact.
func convertBlockMessage(role string, msg prompt.Message) []ChatMessage {
	blocks := msg.Content.GetBlocks()
	out := make([]ChatMessage, 0, len(blocks))
	pending := make([]string, 0, len(blocks))
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, ChatMessage{Role: role, Content: strings.Join(pending, "\n")})
		pending = pending[:0]
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				pending = append(pending, block.Text)
			}
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" {
				continue
			}
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    stringifyToolResult(block.Content),
			})
		}
	}
	flush()
	return out
}

func stringifyToolResult(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []prompt.ContentBlock:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

func compactToolInput(value interface{}) string {
	if value == nil {
		return "{}"
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// normalizeToolDefinitions accepts both OpenAI (`{"type":"function","function":{...}}`)
// and Anthropic (`{"name":...,"input_schema":...}`) tool declarations.
func normalizeToolDefinitions(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			continue
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if fn, ok := decoded["function"].(map[string]interface{}); ok {
			if strings.TrimSpace(stringValue(fn["name"])) == "" {
				continue
			}
			decoded["type"] = "function"
			out = append(out, decoded)
			continue
		}
		name := strings.TrimSpace(stringValue(decoded["name"]))
		if name == "" {
			continue
		}
		parameters := decoded["input_schema"]
		if parameters == nil {
			parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": stringValue(decoded["description"]),
				"parameters":  parameters,
			},
		})
	}
	return out
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
