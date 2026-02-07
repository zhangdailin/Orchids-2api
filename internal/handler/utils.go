package handler

import (
	"net"
	"net/http"
	"regexp"
	"strings"

	"orchids-api/internal/prompt"
)

var envWorkdirRegex = regexp.MustCompile(`(?i)(?:primary\s+)?working directory:\s*([^\n\r]+)`)

func extractWorkdirFromSystem(system []prompt.SystemItem) string {
	for _, item := range system {
		if item.Type == "text" {
			matches := envWorkdirRegex.FindStringSubmatch(item.Text)
			if len(matches) > 1 {
				return strings.TrimSpace(matches[1])
			}
		}
	}
	return ""
}

func channelFromPath(path string) string {
	if strings.HasPrefix(path, "/orchids/") {
		return "orchids"
	}
	if strings.HasPrefix(path, "/warp/") {
		return "warp"
	}
	return ""
}

// mapModel 根据请求的 model 名称映射到 orchids 上游实际支持的模型
// orchids 支持: claude-opus-4-6, claude-opus-4-6-thinking, claude-sonnet-4-5, claude-opus-4-5,
//   claude-sonnet-4-5-thinking, claude-opus-4-5-thinking, claude-haiku-4-5,
//   claude-sonnet-4-20250514, claude-3-7-sonnet-20250219
func mapModel(requestModel string) string {
	lower := strings.ToLower(requestModel)
	isThinking := strings.Contains(lower, "thinking")

	switch {
	// opus-4-6 / opus-4.6 / 4-6-opus 系列
	case strings.Contains(lower, "opus-4-6") || strings.Contains(lower, "opus-4.6") || strings.Contains(lower, "4-6-opus"):
		if isThinking {
			return "claude-opus-4-6-thinking"
		}
		return "claude-opus-4-6"

	// opus-4-5 / opus-4.5 / 4-5-opus 系列
	case strings.Contains(lower, "opus-4-5") || strings.Contains(lower, "opus-4.5") || strings.Contains(lower, "4-5-opus"):
		if isThinking {
			return "claude-opus-4-5-thinking"
		}
		return "claude-opus-4-5"

	// opus 通配 → 最新 4.6
	case strings.Contains(lower, "opus"):
		if isThinking {
			return "claude-opus-4-6-thinking"
		}
		return "claude-opus-4-6"

	// sonnet-3-7 / sonnet-3.7 / 3-7-sonnet 系列
	case strings.Contains(lower, "sonnet-3-7") || strings.Contains(lower, "sonnet-3.7") || strings.Contains(lower, "3-7-sonnet"):
		return "claude-3-7-sonnet-20250219"

	// sonnet-3-5 / sonnet-3.5 / 3-5-sonnet 旧版 → 映射到最新 sonnet
	case strings.Contains(lower, "sonnet-3-5") || strings.Contains(lower, "sonnet-3.5") || strings.Contains(lower, "3-5-sonnet"):
		return "claude-sonnet-4-5"

	// sonnet-4-5 / sonnet-4.5 / 4-5-sonnet 系列
	case strings.Contains(lower, "sonnet-4-5") || strings.Contains(lower, "sonnet-4.5") || strings.Contains(lower, "4-5-sonnet"):
		if isThinking {
			return "claude-sonnet-4-5-thinking"
		}
		return "claude-sonnet-4-5"

	// sonnet-4-20250514 精确匹配
	case strings.Contains(lower, "sonnet-4-20250514"):
		return "claude-sonnet-4-20250514"

	// sonnet-4 / sonnet 通配 → claude-sonnet-4-20250514
	case strings.Contains(lower, "sonnet-4") || strings.Contains(lower, "sonnet"):
		if isThinking {
			return "claude-sonnet-4-5-thinking"
		}
		return "claude-sonnet-4-20250514"

	// haiku-4-5 / haiku-4.5 / 4-5-haiku 系列
	case strings.Contains(lower, "haiku-4-5") || strings.Contains(lower, "haiku-4.5") || strings.Contains(lower, "4-5-haiku"):
		return "claude-haiku-4-5"

	// haiku 通配
	case strings.Contains(lower, "haiku"):
		return "claude-haiku-4-5"

	// 默认
	default:
		return "claude-sonnet-4-5"
	}
}

func conversationKeyForRequest(r *http.Request, req ClaudeRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "conversation_id", "conversationId", "session_id", "sessionId", "thread_id", "threadId", "chat_id", "chatId"); key != "" {
			return key
		}
	}
	if key := headerValue(r, "X-Conversation-Id", "X-Session-Id", "X-Thread-Id", "X-Chat-Id"); key != "" {
		return key
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "user_id", "userId"); key != "" {
			return "user:" + key
		}
	}

	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if host == "" {
		return ""
	}
	ua := strings.TrimSpace(r.UserAgent())
	if ua == "" {
		return host
	}
	return host + "|" + ua
}

func metadataString(metadata map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str, ok := value.(string); ok {
				str = strings.TrimSpace(str)
				if str != "" {
					return str
				}
			}
		}
	}
	return ""
}

func headerValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func extractUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			return strings.TrimSpace(msg.Content.GetText())
		}
		var parts []string
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" {
				text := strings.TrimSpace(block.Text)
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func isPlanMode(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			if containsPlanReminder(msg.Content.GetText()) {
				return true
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" && containsPlanReminder(block.Text) {
				return true
			}
		}
	}
	return false
}

func isSuggestionMode(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			return containsSuggestionMode(msg.Content.GetText())
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" {
				return containsSuggestionMode(block.Text)
			}
		}
		// 最近一条 user 消息没有文本内容，避免回溯旧的 suggestion prompt 误判
		return false
	}
	return false
}

func containsSuggestionMode(text string) bool {
	clean := stripSystemRemindersForMode(text)
	return strings.Contains(strings.ToLower(clean), "suggestion mode")
}

func containsPlanReminder(text string) bool {
	clean := stripSystemRemindersForMode(text)
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "plan mode") || strings.Contains(lower, "planning mode") {
		return true
	}
	if strings.Contains(clean, "计划模式") || strings.Contains(clean, "规划模式") {
		return true
	}
	return false
}

// stripSystemRemindersForMode 移除 <system-reminder>...</system-reminder>，避免误判 plan/suggestion 模式
// 使用 LastIndex 查找结束标签，正确处理嵌套的字面量标签
func stripSystemRemindersForMode(text string) string {
	const startTag = "<system-reminder>"
	const endTag = "</system-reminder>"
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
		blockStart := i + start
		endStart := blockStart + len(startTag)
		end := strings.LastIndex(text[endStart:], endTag)
		if end == -1 {
			sb.WriteString(text[blockStart:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}
