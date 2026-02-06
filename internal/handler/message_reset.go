package handler

import (
	"strings"

	"orchids-api/internal/prompt"
)

// resetMessagesForNewWorkdir 在工作目录切换时清空历史，仅保留当前用户消息。
func resetMessagesForNewWorkdir(messages []prompt.Message) []prompt.Message {
	if len(messages) == 0 {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, "user") {
			return []prompt.Message{messages[i]}
		}
	}
	return []prompt.Message{}
}
