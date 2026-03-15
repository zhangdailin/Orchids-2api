package orchids

import (
	"strings"

	"orchids-api/internal/prompt"
)

const promptProfileOrchidsProtocol = "orchids-protocol"

func BuildCodeFreeMaxPromptAndHistoryWithMeta(
	messages []prompt.Message,
	system []prompt.SystemItem,
	noThinking bool,
) (string, []map[string]string, PromptBuildMeta) {
	conversation := buildOrchidsConversationMessages(messages)
	currentUserIdx := findCurrentOrchidsUserMessageIndex(conversation)

	promptText := buildCodeFreeMaxPrompt(messages, system, conversation)
	history := buildOrchidsConversationHistory(conversation, currentUserIdx)

	return promptText, history, PromptBuildMeta{
		Profile:    promptProfileOrchidsProtocol,
		NoThinking: noThinking,
	}
}

func buildCodeFreeMaxPrompt(
	messages []prompt.Message,
	system []prompt.SystemItem,
	conversation []OrchidsConversationMessage,
) string {
	systemText := extractCodeFreeMaxSystemText(messages, system)

	userText, _ := extractOrchidsUserMessage(conversation)
	userText = strings.TrimSpace(userText)

	var b strings.Builder
	if systemText != "" {
		b.WriteString("<sys>\n")
		b.WriteString(systemText)
		b.WriteString("\n</sys>\n\n")
	}
	if userText != "" {
		b.WriteString("<user>\n")
		b.WriteString(userText)
		b.WriteString("\n</user>\n")
	}
	return strings.TrimSpace(b.String())
}

func extractCodeFreeMaxSystemText(messages []prompt.Message, system []prompt.SystemItem) string {
	var parts []string
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "system") {
			continue
		}
		if msg.Content.IsString() {
			text := strings.TrimSpace(msg.Content.GetText())
			if text != "" {
				parts = append(parts, text)
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if strings.TrimSpace(block.Type) != "text" {
				continue
			}
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 && len(system) > 0 {
		for _, item := range system {
			text := strings.TrimSpace(item.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}
