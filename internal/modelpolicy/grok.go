package modelpolicy

import (
	"slices"
	"strings"
)

var stableGrokTextModelIDs = []string{
	"grok-4.3",
	"grok-4.3-latest",
	"grok-latest",
	"grok-420",
	"grok-3-mini",
	"grok-4-thinking",
	"grok-4.1-expert",
}

var publicGrokModelIDs = []string{
	"grok-4.3",
	"grok-4.3-latest",
	"grok-latest",
	"grok-420",
	"grok-3-mini",
	"grok-4-thinking",
	"grok-4.1-expert",
	"grok-imagine-image-lite",
	"grok-imagine-image",
	"grok-imagine-image-pro",
	"grok-imagine-image-edit",
	"grok-imagine-video",
}

var stableGrokTextModelAllowlist = func() map[string]struct{} {
	out := make(map[string]struct{}, len(stableGrokTextModelIDs))
	for _, id := range stableGrokTextModelIDs {
		out[id] = struct{}{}
	}
	return out
}()

var publicGrokModelAllowlist = func() map[string]struct{} {
	out := make(map[string]struct{}, len(publicGrokModelIDs))
	for _, id := range publicGrokModelIDs {
		out[id] = struct{}{}
	}
	return out
}()

func IsPublicGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := publicGrokModelAllowlist[id]
	return ok
}

func IsStableGrokTextModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := stableGrokTextModelAllowlist[id]
	return ok
}

func StableGrokTextModelIDs() []string {
	return slices.Clone(stableGrokTextModelIDs)
}

func PublicGrokModelIDs() []string {
	return slices.Clone(publicGrokModelIDs)
}

func IsVisibleGrokModel(modelID string, verified bool) bool {
	return IsPublicGrokModelID(modelID) || verified
}
