package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

type warpContinuation struct {
	conversationID string
	accountID      int64
	toolContexts   map[string]upstream.WarpToolContext
	taskContext    []byte
}

func warpBindingInput(toolType, input string) string {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "run_shell_command", "run_command", "read_files", "read_file", "read_shell_command_output":
		return input
	default:
		return ""
	}
}

func (h *Handler) resolveWarpContinuation(ctx context.Context, conversationKey string, messages []prompt.Message) (warpContinuation, error) {
	continuation := warpContinuation{toolContexts: make(map[string]upstream.WarpToolContext)}
	if conversationKey != "" {
		continuation.conversationID, _ = h.sessionStore.GetConvID(ctx, conversationKey)
		continuation.accountID, _ = h.sessionStore.GetAccountID(ctx, conversationKey)
		if encoded, ok := h.sessionStore.GetWarpTaskContext(ctx, conversationKey); ok {
			decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
			if err != nil {
				return warpContinuation{}, fmt.Errorf("cannot resume Warp conversation because its task context is invalid")
			}
			continuation.taskContext = decoded
		}
		h.sessionStore.Touch(ctx, conversationKey)
	}

	toolResultIDs := latestToolResultIDs(messages)
	for _, toolCallID := range toolResultIDs {
		binding, ok := h.sessionStore.GetWarpToolBinding(ctx, conversationKey, toolCallID)
		if !ok {
			return warpContinuation{}, fmt.Errorf("cannot resume Warp tool result %q because its conversation binding has expired or is unavailable", toolCallID)
		}
		if continuation.conversationID != "" && continuation.conversationID != binding.ConversationID {
			return warpContinuation{}, fmt.Errorf("tool result %q belongs to a different Warp conversation", toolCallID)
		}
		if continuation.accountID != 0 && binding.AccountID != 0 && continuation.accountID != binding.AccountID {
			return warpContinuation{}, fmt.Errorf("tool result %q belongs to a different Warp account", toolCallID)
		}
		continuation.conversationID = binding.ConversationID
		if binding.AccountID != 0 {
			continuation.accountID = binding.AccountID
		}
		continuation.toolContexts[toolCallID] = upstream.WarpToolContext{
			Type:  binding.ToolType,
			Name:  binding.ToolName,
			Input: binding.ToolInput,
		}
		if encoded := strings.TrimSpace(binding.TaskContext); encoded != "" {
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				return warpContinuation{}, fmt.Errorf("cannot resume Warp tool result %q because its task context is invalid", toolCallID)
			}
			if len(continuation.taskContext) > 0 && string(continuation.taskContext) != string(decoded) {
				return warpContinuation{}, fmt.Errorf("warp tool results belong to different task contexts")
			}
			continuation.taskContext = decoded
		}
	}
	if len(toolResultIDs) > 0 && continuation.conversationID == "" {
		return warpContinuation{}, fmt.Errorf("cannot resume Warp tool results because the issuing conversation has expired or is unavailable")
	}

	if conversationKey != "" && continuation.conversationID != "" {
		h.sessionStore.SetConvID(ctx, conversationKey, continuation.conversationID)
		if continuation.accountID != 0 {
			h.sessionStore.SetAccountID(ctx, conversationKey, continuation.accountID)
		}
	}
	return continuation, nil
}

func latestToolResultIDs(messages []prompt.Message) []string {
	var reversed []string
	foundPendingInput := false
	seen := make(map[string]struct{})
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Role))
		if role != "user" && role != "tool" {
			if foundPendingInput {
				break
			}
			continue
		}
		foundPendingInput = true
		if messages[i].Content.IsString() {
			continue
		}
		blocks := messages[i].Content.GetBlocks()
		for j := len(blocks) - 1; j >= 0; j-- {
			block := blocks[j]
			if block.Type != "tool_result" {
				continue
			}
			id := strings.TrimSpace(block.ToolUseID)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			reversed = append(reversed, id)
		}
	}
	ids := make([]string, len(reversed))
	for i := range reversed {
		ids[len(reversed)-1-i] = reversed[i]
	}
	return ids
}
