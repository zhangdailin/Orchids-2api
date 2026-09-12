package modelpolicy

import (
	"slices"
	"strings"
)

// DefaultWorkBuddyModelID is the gateway-side default for the WorkBuddy
// international channel. It is the first entry of the upstream `cli` agent
// whitelist, and unlike the reasoning-only models next to it, it answers
// without forcing a thinking phase.
const DefaultWorkBuddyModelID = "default-model"

// latestWorkBuddyModelIDs is the verified `cli` agent whitelist of the
// WorkBuddy international backend (www.workbuddy.ai, isOversea=true), in the
// order the upstream reports it. The full catalog is discoverable at any time
// through Admin → Models → refresh (GET /v3/config); this list only seeds the
// catalog so the channel is usable before the first discovery run.
var latestWorkBuddyModelIDs = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"glm-5.2",
	"kimi-k3",
	"kimi-k2.6",
}

var latestWorkBuddyModelAllowlist = stringSet(latestWorkBuddyModelIDs)

// LatestWorkBuddyModelIDs returns the seeded catalog in upstream order.
func LatestWorkBuddyModelIDs() []string {
	return slices.Clone(latestWorkBuddyModelIDs)
}

// IsLatestWorkBuddyModelID reports whether the id belongs to the seeded set.
// Refreshed catalogs may legitimately contain ids outside this list.
func IsLatestWorkBuddyModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	_, ok := latestWorkBuddyModelAllowlist[id]
	return ok
}
