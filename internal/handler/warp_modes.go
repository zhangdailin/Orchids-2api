package handler

import (
	"strings"

	"orchids-api/internal/warp"
)

// warp-chat and warp-agent used to be synthetic virtual models that selected a
// Warp conversation mode. They are gone: Warp requests route on the concrete
// model IDs returned by upstream account discovery, so all that remains of them
// is the id predicate that keeps stale persisted rows out of the catalog and
// off the by-id endpoint.
const (
	warpChatModelID  = "warp-chat"
	warpAgentModelID = "warp-agent"
)

func normalizeWarpPublicModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

func isWarpVirtualModel(modelID string) bool {
	normalized := normalizeWarpPublicModelID(modelID)
	return normalized == warpChatModelID || normalized == warpAgentModelID
}

func upstreamWarpModelID(modelID string) string {
	if isWarpVirtualModel(modelID) {
		return warp.DefaultModel()
	}
	return strings.TrimSpace(modelID)
}
