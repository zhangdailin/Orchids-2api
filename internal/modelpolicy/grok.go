package modelpolicy

import "strings"

// DeprecatedGrokModelIDs lists model identifiers retired from the Grok
// channel. The store's startup cleanup derives its Grok entries from this map,
// so this is the single source of truth.
var DeprecatedGrokModelIDs = map[string]struct{}{
	"grok-4.20-0309-non-reasoning":       {},
	"grok-4.20-0309":                     {},
	"grok-4.20-0309-reasoning":           {},
	"grok-4.20-0309-non-reasoning-super": {},
	"grok-4.20-0309-super":               {},
	"grok-4.20-0309-reasoning-super":     {},
	"grok-4.20-0309-non-reasoning-heavy": {},
	"grok-4.20-0309-heavy":               {},
	"grok-4.20-0309-reasoning-heavy":     {},
	"grok-4.20-multi-agent-0309":         {},
	"grok-4.20-fast":                     {},
	"grok-4.20-auto":                     {},
	"grok-4.20-expert":                   {},
	"grok-4.20-heavy":                    {},
	"grok-4.3-beta":                      {},
	"grok-build-0.1":                     {},
	"grok-imagine-image-pro":             {},
	"grok-3":                             {},
	"grok-3-thinking":                    {},
	"grok-3-fast":                        {},
	"grok-4":                             {},
	"grok-4-mini":                        {},
	"grok-4-fast":                        {},
	"grok-4-heavy":                       {},
	"grok-4.1":                           {},
	"grok-4.1-mini":                      {},
	"grok-4.1-fast":                      {},
	"grok-4.1-thinking":                  {},
	"grok-4-1-thinking":                  {},
	"grok-4-1-thinking-1129":             {},
	"grok-4.2":                           {},
	"grok-4-2":                           {},
	"grok-4.20-beta":                     {},
	"grok-4-20-beta":                     {},
	"grok-4.20-reasoning":                {},
	"grok-4.20-non-reasoning":            {},
	"grok-4.20-multi-agent":              {},
	"grok-420":                           {},
	// grok-4.3 and grok-build-0.1 are supported via console.x.ai only
	"grok-code-fast":         {},
	"grok-code-fast-1":       {},
	"grok-imagine-1.0":       {},
	"grok-imagine-1.0-fast":  {},
	"grok-imagine-1.0-edit":  {},
	"grok-imagine-1.0-video": {},
	"grok-2":                 {},
	"grok-2.1":               {},
	"grok-3.1":               {},
	"grok-4.21":              {},
}

func IsDeprecatedGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	_, ok := DeprecatedGrokModelIDs[id]
	return ok
}

// IsVisibleGrokModel reports whether a Grok model may be served.
//
// Visibility follows the account capability snapshot: a model is visible only
// when a refresh observed it for an active account (verified). There is
// deliberately no compiled-in allowlist — it would advertise models no account
// ever reported, and it would keep a withdrawn model visible.
func IsVisibleGrokModel(modelID string, verified bool) bool {
	if IsDeprecatedGrokModelID(modelID) {
		return false
	}
	return verified
}
