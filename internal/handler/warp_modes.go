package handler

import (
	"strings"

	"orchids-api/internal/warp"
)

const (
	warpChatModelID  = "warp-chat"
	warpAgentModelID = "warp-agent"
)

func normalizeWarpPublicModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

func isWarpChatModel(modelID string) bool {
	return normalizeWarpPublicModelID(modelID) == warpChatModelID
}

func isWarpAgentModel(modelID string) bool {
	return normalizeWarpPublicModelID(modelID) == warpAgentModelID
}

func isWarpVirtualModel(modelID string) bool {
	return isWarpChatModel(modelID) || isWarpAgentModel(modelID)
}

func upstreamWarpModelID(modelID string) string {
	if isWarpVirtualModel(modelID) {
		return warp.DefaultModel()
	}
	return strings.TrimSpace(modelID)
}

func warpChatToolGateMessage() string {
	return "Answer directly in text only. Do not call tools, do not create or edit files, do not run commands, and do not use Warp agent mode. If the user asks for code, provide the code in the response."
}
