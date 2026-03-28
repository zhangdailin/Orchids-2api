package handler

import (
	"context"
	"log/slog"
	"strings"

	"orchids-api/internal/bolt"
	"orchids-api/internal/prompt"
)

func (h *Handler) restoreBoltTools(ctx context.Context, conversationKey string) []interface{} {
	if h == nil || h.sessionStore == nil || conversationKey == "" {
		return nil
	}
	names, ok := h.sessionStore.GetBoltToolNames(ctx, conversationKey)
	if !ok || len(names) == 0 {
		return nil
	}
	return minimalIncomingToolsFromNames(names)
}

func (h *Handler) persistBoltTools(ctx context.Context, conversationKey string, tools []interface{}) {
	if h == nil || h.sessionStore == nil || conversationKey == "" {
		return
	}
	names := supportedToolNames(tools)
	if len(names) == 0 {
		return
	}
	h.sessionStore.SetBoltToolNames(ctx, conversationKey, names)
}

func minimalIncomingToolsFromNames(names []string) []interface{} {
	specs := bolt.MinimalSupportedToolSpecs(names)
	if len(specs) == 0 {
		return nil
	}
	normalized := make([]interface{}, 0, len(specs))
	for _, spec := range specs {
		normalized = append(normalized, spec)
	}
	return normalized
}

func inferBoltToolsFromMessages(messages []prompt.Message) []interface{} {
	if len(messages) == 0 {
		return nil
	}

	names := make([]string, 0, 8)
	for _, msg := range messages {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "tool_use" {
				continue
			}
			if name := strings.TrimSpace(block.Name); name != "" {
				names = append(names, name)
			}
		}
	}

	return minimalIncomingToolsFromNames(names)
}

func logBoltToolsRestored(conversationKey string, tools []interface{}) {
	slog.Debug(
		"bolt tools restored from session",
		"conversation_id", conversationKey,
		"tool_names", supportedToolNames(tools),
	)
}

func logBoltToolsInferred(tools []interface{}) {
	slog.Debug(
		"bolt tools inferred from message history",
		"tool_names", supportedToolNames(tools),
	)
}
