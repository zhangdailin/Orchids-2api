package promptbuilder

import (
	"strings"

	"orchids-api/internal/prompt"
)

const (
	thinkingBudget  = 10000
	thinkingMin     = 1024
	thinkingMax     = 128000
	thinkingModeTag = "<thinking_mode>"
	thinkingLenTag  = "<max_thinking_length>"

	profileDefault  = "default"
	profileUltraMin = "ultra-min"
)

// Meta captures the prompt builder decisions that handler code needs to keep.
type Meta struct {
	Profile    string
	NoThinking bool
}

func BuildWithMeta(messages []prompt.Message, system []prompt.SystemItem, model string, noThinking bool, workdir string, maxTokens int) (string, []map[string]string, Meta) {
	return BuildWithMetaAndTools(messages, system, model, noThinking, workdir, maxTokens, nil)
}

func BuildWithMetaAndTools(messages []prompt.Message, system []prompt.SystemItem, model string, noThinking bool, workdir string, maxTokens int, tools []interface{}) (string, []map[string]string, Meta) {
	meta := Meta{Profile: profileDefault}

	systemText := extractSystemPrompt(messages)
	if strings.TrimSpace(systemText) == "" && len(system) > 0 {
		var sb strings.Builder
		for _, item := range system {
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			sb.WriteString(item.Text)
			sb.WriteString("\n")
		}
		systemText = sb.String()
	}
	systemText = stripSystemReminders(systemText)

	userText := stripSystemReminders(extractWarpUserMessage(messages))
	currentUserIdx := findCurrentUserMessageIndex(messages)
	userText = resolveCurrentUserTurnText(messages, currentUserIdx, userText)
	currentTurnToolResultOnly := isToolResultOnlyUserMessage(messages, currentUserIdx)
	supportedTools := supportedToolNames(tools)
	if currentTurnToolResultOnly && len(supportedTools) == 0 {
		userText = rewriteToolResultFollowUpForDirectAnswer(userText)
	}

	var historyMessages []prompt.Message
	if currentUserIdx >= 0 {
		historyMessages = messages[:currentUserIdx]
	} else {
		historyMessages = messages
	}
	chatHistory := convertWarpChatHistory(historyMessages)
	chatHistory = pruneExploratoryAssistantHistory(chatHistory, currentTurnToolResultOnly, len(supportedTools) == 0)

	meta.Profile = selectPromptProfileForTurn(userText, currentTurnToolResultOnly)
	meta.NoThinking = noThinking || currentTurnToolResultOnly || shouldDisableThinkingForProfile(meta.Profile)

	promptText := buildLocalAssistantPromptWithProfileAndTools(systemText, userText, model, workdir, maxTokens, meta.Profile, tools)
	if !meta.NoThinking && !isSuggestionModeText(userText) {
		promptText = injectThinkingPrefix(promptText)
	}

	promptText, chatHistory = enforceWarpPromptBudget(promptText, chatHistory, maxTokens)
	return promptText, chatHistory, meta
}
