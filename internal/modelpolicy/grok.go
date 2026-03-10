package modelpolicy

import "strings"

var publicGrokModelAllowlist = map[string]struct{}{
	"grok-3":                 {},
	"grok-3-mini":            {},
	"grok-3-thinking":        {},
	"grok-4":                 {},
	"grok-4-mini":            {},
	"grok-4-thinking":        {},
	"grok-4-heavy":           {},
	"grok-4.1-mini":          {},
	"grok-4.1-fast":          {},
	"grok-4.1-expert":        {},
	"grok-4.1-thinking":      {},
	"grok-420":               {},
	"grok-imagine-1.0":       {},
	"grok-imagine-1.0-fast":  {},
	"grok-imagine-1.0-edit":  {},
	"grok-imagine-1.0-video": {},
}

func IsPublicGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	_, ok := publicGrokModelAllowlist[id]
	return ok
}

func IsVisibleGrokModel(modelID string, verified bool) bool {
	return IsPublicGrokModelID(modelID) || verified
}
