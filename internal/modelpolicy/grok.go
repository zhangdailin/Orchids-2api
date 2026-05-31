package modelpolicy

import (
	"slices"
	"strings"
)

var publicGrokModelIDs = []string{
	"grok-4.20-0309-non-reasoning",
	"grok-4.20-0309",
	"grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning-super",
	"grok-4.20-0309-super",
	"grok-4.20-0309-reasoning-super",
	"grok-4.20-0309-non-reasoning-heavy",
	"grok-4.20-0309-heavy",
	"grok-4.20-0309-reasoning-heavy",
	"grok-4.20-multi-agent-0309",
	"grok-4.20-fast",
	"grok-4.20-auto",
	"grok-4.20-expert",
	"grok-4.20-heavy",
	"grok-4.3",
	"grok-build-0.1",
	"grok-imagine-image-lite",
	"grok-imagine-image",
	"grok-imagine-image-pro",
	"grok-imagine-image-edit",
	"grok-imagine-video",
}

var publicGrokModelAllowlist = func() map[string]struct{} {
	out := make(map[string]struct{}, len(publicGrokModelIDs))
	for _, id := range publicGrokModelIDs {
		out[id] = struct{}{}
	}
	return out
}()

var deprecatedGrokModelIDs = map[string]struct{}{
	"grok-4.3-beta": {},
}

func IsDeprecatedGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := deprecatedGrokModelIDs[id]
	return ok
}

func IsPublicGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := publicGrokModelAllowlist[id]
	return ok
}

func PublicGrokModelIDs() []string {
	return slices.Clone(publicGrokModelIDs)
}

func IsVisibleGrokModel(modelID string, verified bool) bool {
	if IsDeprecatedGrokModelID(modelID) {
		return false
	}
	return IsPublicGrokModelID(modelID) || verified
}
