package puter

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

// Puter's DeepSeek gateway requires a non-empty reasoning_content on every
// replayed assistant message. U+200B satisfies that schema requirement without
// injecting visible instructions into long legacy conversations whose original
// thinking was never persisted.
const missingDeepSeekReasoningFallback = "\u200b"

// convertMessages 将 OpenAI 风格消息转换为 Puter 消息。系统条目逐字透传为独立
// system 消息，其余消息按角色/schema 映射。echoReasoning 仅在目标服务开启思考模式
// 且要求回传 reasoning_content（deepseek）时为 true，此时保留 assistant 消息的推理
// 内容；其余服务行为不变（与旧版一致）。
func convertMessages(messages []prompt.Message, system []prompt.SystemItem, echoReasoning bool) []message {
	out := make([]message, 0, len(messages)+len(system))
	pendingToolCalls := make(map[string]bool)
	for _, item := range system {
		if text := strings.TrimSpace(item.Text); text != "" {
			out = append(out, message{Role: "system", Content: item.Text})
		}
	}
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "" {
			role = "user"
		}
		if msg.Content.IsString() {
			if text := msg.Content.GetText(); strings.TrimSpace(text) != "" {
				m := message{Role: role, Content: text}
				if echoReasoning && role == "assistant" {
					m.ReasoningContent = strings.TrimSpace(msg.ReasoningContent)
					if m.ReasoningContent == "" {
						m.ReasoningContent = missingDeepSeekReasoningFallback
					}
				}
				out = append(out, m)
			}
			continue
		}
		switch role {
		case "assistant":
			if converted, ok := convertAssistantMessage(msg, echoReasoning); ok {
				for _, call := range converted.ToolCalls {
					pendingToolCalls[call.ID] = true
				}
				out = append(out, converted)
			}
		case "user", "tool":
			out = append(out, convertUserBlocks(msg.Content.GetBlocks(), pendingToolCalls)...)
		default:
			if text := joinTextBlocks(msg.Content.GetBlocks()); text != "" {
				out = append(out, message{Role: role, Content: text})
			}
		}
	}
	return out
}

// convertAssistantMessage 把 assistant 消息的文本/tool_use 块映射到 puter message。
// echoReasoning 时，OpenAI 风格 `reasoning_content` 顶层字段与 Anthropic 风格
// `thinking` 块都会汇入 message.ReasoningContent，保证 DeepSeek 思考模式能回传推理内容。
func convertAssistantMessage(msg prompt.Message, echoReasoning bool) (message, bool) {
	message := message{Role: "assistant"}
	if echoReasoning {
		message.ReasoningContent = strings.TrimSpace(msg.ReasoningContent)
	}
	var text []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case "thinking":
			if echoReasoning && message.ReasoningContent == "" {
				message.ReasoningContent = strings.TrimSpace(block.Thinking)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = newToolCallID()
			}
			message.ToolCalls = append(message.ToolCalls, toolCall{
				ID:   id,
				Type: "function",
				Function: toolCallFunction{
					Name:      name,
					Arguments: util.CompactToolInput(block.Input),
				},
			})
		}
	}
	message.Content = strings.Join(text, "\n")
	if echoReasoning && message.ReasoningContent == "" && (message.Content != "" || len(message.ToolCalls) > 0) {
		message.ReasoningContent = missingDeepSeekReasoningFallback
	}
	return message, message.Content != "" || len(message.ToolCalls) > 0 || message.ReasoningContent != ""
}

func convertUserBlocks(blocks []prompt.ContentBlock, pendingToolCalls map[string]bool) []message {
	out := make([]message, 0, len(blocks))
	var text []string
	flushText := func() {
		if len(text) == 0 {
			return
		}
		out = append(out, message{Role: "user", Content: strings.Join(text, "\n")})
		text = text[:0]
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case "tool_result":
			flushText()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" {
				continue
			}
			if !pendingToolCalls[toolID] {
				continue
			}
			delete(pendingToolCalls, toolID)
			out = append(out, message{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    stringifyToolResult(block.Content),
			})
		}
	}
	flushText()
	return out
}

func joinTextBlocks(blocks []prompt.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func stringifyToolResult(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []prompt.ContentBlock:
		return joinTextBlocks(typed)
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

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
			if strings.TrimSpace(util.StringValue(fn["name"])) == "" {
				continue
			}
			decoded["type"] = "function"
			out = append(out, decoded)
			continue
		}
		name := strings.TrimSpace(util.StringValue(decoded["name"]))
		if name == "" {
			if strings.TrimSpace(util.StringValue(decoded["type"])) != "" {
				out = append(out, decoded)
			}
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
				"description": util.StringValue(decoded["description"]),
				"parameters":  parameters,
			},
		})
	}
	return out
}

func mergeAdjacentAssistantMessages(messages []message) []message {
	if len(messages) < 2 {
		return messages
	}
	out := make([]message, 0, len(messages))
	for _, message := range messages {
		if message.Role != "assistant" || len(out) == 0 || out[len(out)-1].Role != "assistant" {
			out = append(out, message)
			continue
		}
		previous := &out[len(out)-1]
		previous.Content = joinAssistantReplayText(previous.Content, message.Content)
		previous.ReasoningContent = joinAssistantReplayReasoning(previous.ReasoningContent, message.ReasoningContent)
		previous.ToolCalls = append(previous.ToolCalls, message.ToolCalls...)
	}
	return out
}

func joinAssistantReplayText(left, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + "\n" + right
	}
}

func joinAssistantReplayReasoning(left, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == missingDeepSeekReasoningFallback {
		left = ""
	}
	if right == missingDeepSeekReasoningFallback {
		right = ""
	}
	joined := joinAssistantReplayText(left, right)
	if joined == "" {
		return missingDeepSeekReasoningFallback
	}
	return joined
}

// splitMultiToolCalls 把单个 assistant 消息里的多个 tool_calls 拆成多段
// "assistant(单个 tool_call) → tool(对应回应)"。puter 的 DeepSeekProvider
// 会在每个 tool 消息后注入一条 system 消息;若一个 assistant 消息带多个
// tool_calls,注入的 system 会插在 tool 回应之间,而 DeepSeek 的严格校验
// 要求 assistant(tool_calls) 后只能跟 tool 消息直到全部 tool_call_id 被回应,
// 于是报 "insufficient tool messages following tool_calls message"。
// 拆成单 tool_call 序列后,每个 assistant 的回应在注入前就已完整。
func splitMultiToolCalls(messages []message) []message {
	out := make([]message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) < 2 {
			out = append(out, m)
			continue
		}
		j := i + 1
		results := make([]message, 0, len(m.ToolCalls))
		for j < len(messages) && messages[j].Role == "tool" {
			results = append(results, messages[j])
			j++
		}
		byID := make(map[string]message, len(m.ToolCalls))
		for _, r := range results {
			byID[r.ToolCallID] = r
		}
		if len(byID) != len(m.ToolCalls) {
			// 回应不齐时保持原样,不擅自改写。
			out = append(out, m)
			continue
		}
		for k, tc := range m.ToolCalls {
			part := message{Role: "assistant", ReasoningContent: m.ReasoningContent, ToolCalls: []toolCall{tc}}
			if k == 0 {
				part.Content = m.Content
			}
			out = append(out, part)
			if res, ok := byID[tc.ID]; ok {
				out = append(out, res)
			}
		}
		i = j - 1
	}
	return out
}
