package modelpolicy

import "strings"

// reasoningEffortCapabilities maps an upstream model slug to the effort levels
// its wire contract accepts. A slug that is absent falls back to "none" only,
// so an unknown model never advertises a level upstream may reject.
var reasoningEffortCapabilities = map[string][]string{
	"grok-4.5":                     {"low", "medium", "high"},
	"grok-4.6":                     {"low", "medium", "high", "xhigh"},
	"grok-4.3":                     {"none", "low", "medium", "high"},
	"grok-build-0.1":               {"none"},
	"grok-4.20-0309-reasoning":     {"low", "medium", "high"},
	"grok-4.20-0309-non-reasoning": {"none"},
	"grok-4.20-multi-agent-0309":   {"low", "medium", "high", "xhigh"},
	"grok-3-mini":                  {"low", "medium", "high"},
	"grok-3-mini-fast":             {"low", "medium", "high"},
	"grok-composer-2.5-fast":       {"none"},
}

// consoleFixedReasoningModels produce reasoning but reject an effort parameter.
// The same slug on another plane may expose a different contract, so the
// restriction stays provider-scoped.
var consoleFixedReasoningModels = map[string]struct{}{
	"grok-4.20-0309-reasoning": {},
}

// GrokModelSlug strips any provider prefix ("console/grok-4.5" -> "grok-4.5").
func GrokModelSlug(publicID string) string {
	slug := strings.ToLower(strings.TrimSpace(publicID))
	if index := strings.LastIndex(slug, "/"); index >= 0 {
		slug = slug[index+1:]
	}
	return slug
}

// IsConsoleGrokModel reports whether the public ID routes to the Console plane.
func IsConsoleGrokModel(publicID string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(publicID)), "console/")
}

// IsGrokComposerModel reports whether the model belongs to the Composer family,
// which never accepts a configurable reasoning effort.
func IsGrokComposerModel(publicID string) bool {
	return strings.HasPrefix(GrokModelSlug(publicID), "grok-composer-")
}

// SupportedReasoningEfforts returns the configurable effort levels for a model.
// An empty result means the model reasons intrinsically but exposes no
// configurable effort parameter.
func SupportedReasoningEfforts(publicID string) []string {
	slug := GrokModelSlug(publicID)
	if IsConsoleGrokModel(publicID) {
		if _, fixed := consoleFixedReasoningModels[slug]; fixed {
			return nil
		}
	}
	if levels, ok := reasoningEffortCapabilities[slug]; ok {
		return append([]string(nil), levels...)
	}
	return []string{"none"}
}

// SupportsReasoningEffort reports whether the model accepts the given level.
func SupportsReasoningEffort(publicID, effort string) bool {
	effort = strings.ToLower(strings.TrimSpace(effort))
	for _, level := range SupportedReasoningEfforts(publicID) {
		if level == effort {
			return true
		}
	}
	return false
}
