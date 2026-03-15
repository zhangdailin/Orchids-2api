package orchids

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/prompt"
)

type OrchidsConversationMessage struct {
	Role        string
	ContentType string
	Content     interface{}
}

func buildOrchidsConversationMessages(messages []prompt.Message) []OrchidsConversationMessage {
	if len(messages) == 0 {
		return nil
	}

	out := make([]OrchidsConversationMessage, 0, len(messages))
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if msg.Content.IsString() {
			out = append(out, OrchidsConversationMessage{
				Role:        role,
				ContentType: "string",
				Content:     msg.Content.GetText(),
			})
			continue
		}

		blocks := msg.Content.GetBlocks()
		converted := make([]interface{}, 0, len(blocks))
		for _, block := range blocks {
			converted = append(converted, promptContentBlockToOrchidsMap(block))
		}
		out = append(out, OrchidsConversationMessage{
			Role:        role,
			ContentType: "slice",
			Content:     converted,
		})
	}
	return out
}

func buildOrchidsConversationHistory(messages []OrchidsConversationMessage, currentUserIdx int) []map[string]string {
	if len(messages) == 0 {
		return nil
	}

	limit := len(messages)
	if currentUserIdx >= 0 {
		limit = currentUserIdx
	}

	history := make([]map[string]string, 0, limit)
	for i := 0; i < limit; i++ {
		msg := messages[i]
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		text, _ := extractOrchidsMessageContent(msg.Content, msg.ContentType)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		history = append(history, map[string]string{
			"role":    role,
			"content": text,
		})
	}

	if len(history) == 0 {
		return nil
	}
	return history
}

func extractOrchidsUserMessage(messages []OrchidsConversationMessage) (string, bool) {
	if len(messages) == 0 {
		return "", false
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		text, toolResultOnly := extractOrchidsMessageContent(msg.Content, msg.ContentType)
		text = strings.TrimSpace(text)
		if text != "" || toolResultOnly {
			return text, toolResultOnly
		}
	}

	return "", false
}

func findCurrentOrchidsUserMessageIndex(messages []OrchidsConversationMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(messages[i].Role)) != "user" {
			continue
		}
		text, toolResultOnly := extractOrchidsMessageContent(messages[i].Content, messages[i].ContentType)
		if strings.TrimSpace(text) != "" || toolResultOnly {
			return i
		}
	}
	return -1
}

func promptContentBlockToOrchidsMap(block prompt.ContentBlock) map[string]interface{} {
	m := map[string]interface{}{
		"type": strings.TrimSpace(block.Type),
	}

	switch strings.TrimSpace(block.Type) {
	case "text":
		m["text"] = block.Text
	case "image", "document":
		source := map[string]interface{}{}
		if block.Source != nil {
			if strings.TrimSpace(block.Source.MediaType) != "" {
				source["media_type"] = block.Source.MediaType
			}
			if strings.TrimSpace(block.Source.Data) != "" {
				source["data"] = block.Source.Data
			}
			if strings.TrimSpace(block.Source.URL) != "" {
				source["url"] = block.Source.URL
			}
		}
		if len(source) > 0 {
			m["source"] = source
		}
		if strings.TrimSpace(block.URL) != "" {
			m["url"] = block.URL
		}
	case "tool_use":
		m["id"] = block.ID
		m["name"] = block.Name
		m["input"] = block.Input
	case "tool_result":
		m["tool_use_id"] = block.ToolUseID
		m["content"] = block.Content
		if block.IsError {
			m["is_error"] = true
		}
	}

	return m
}

func extractOrchidsMessageContent(content interface{}, contentType string) (string, bool) {
	switch contentType {
	case "string":
		if text, ok := content.(string); ok {
			return strings.TrimSpace(text), false
		}
		return "", false
	case "slice":
		blocks, ok := content.([]interface{})
		if !ok {
			return "", false
		}

		var parts []string
		hasToolResult := false
		hasNonToolText := false

		for _, raw := range blocks {
			block, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			blockType, _ := block["type"].(string)
			switch strings.TrimSpace(blockType) {
			case "thinking", "reasoning", "input_text":
				continue
			case "text":
				text, _ := block["text"].(string)
				text = strings.TrimSpace(text)
				if strings.Contains(text, "<search_quality_reflection>") || text == "" {
					continue
				}
				parts = append(parts, text)
				hasNonToolText = true
			case "image":
				if formatted := formatOrchidsConversationMediaBlock(block, "image"); formatted != "" {
					parts = append(parts, formatted)
					hasNonToolText = true
				}
			case "document":
				if formatted := formatOrchidsConversationMediaBlock(block, "document"); formatted != "" {
					parts = append(parts, formatted)
					hasNonToolText = true
				}
			case "tool_use":
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				inputStr := ""
				if input, ok := block["input"]; ok {
					if rawInput, err := json.Marshal(input); err == nil {
						inputStr = string(rawInput)
					}
				}
				parts = append(parts, fmt.Sprintf(`<tool_use id="%s" name="%s" input="%s">`, id, name, inputStr))
				hasNonToolText = true
			case "tool_result":
				hasToolResult = true
			}
		}

		return strings.TrimSpace(strings.Join(parts, "\n")), hasToolResult && !hasNonToolText
	default:
		return "", false
	}
}

func formatOrchidsConversationMediaBlock(block map[string]interface{}, kind string) string {
	if source, ok := block["source"].(map[string]interface{}); ok {
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		mediaType = strings.TrimSpace(mediaType)
		data = strings.TrimSpace(data)
		if data != "" {
			if kind == "image" {
				return fmt.Sprintf("![image](data:%s;base64,%s)", mediaType, data)
			}
			return fmt.Sprintf("[document](data:%s)", data)
		}
	}

	return ""
}

func extractOrchidsAttachmentURLs(messages []prompt.Message) []string {
	seen := make(map[string]struct{})
	urls := make([]string, 0, 4)
	for _, msg := range messages {
		if msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			blockType := strings.TrimSpace(block.Type)
			if blockType != "image" && blockType != "document" {
				continue
			}
			url := ""
			if block.Source != nil {
				url = strings.TrimSpace(block.Source.URL)
			}
			if url == "" {
				url = strings.TrimSpace(block.URL)
			}
			if url == "" {
				continue
			}
			if _, ok := seen[url]; ok {
				continue
			}
			seen[url] = struct{}{}
			urls = append(urls, url)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return urls
}
