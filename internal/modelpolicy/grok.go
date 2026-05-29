package modelpolicy

import (
	"slices"
	"strings"
)

const (
	grokTierBasic = iota
	grokTierSuper
	grokTierHeavy
)

type grokModelRouting struct {
	tier       int
	preferBest bool
}

var stableGrokTextModelIDs = []string{
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
	"grok-4.3-beta",
}

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
	"grok-4.3-beta",
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

var grokModelRoutingByID = map[string]grokModelRouting{
	"grok-4.20-0309-non-reasoning":       {tier: grokTierBasic},
	"grok-4.20-0309":                     {tier: grokTierSuper},
	"grok-4.20-0309-reasoning":           {tier: grokTierSuper},
	"grok-4.20-0309-non-reasoning-super": {tier: grokTierSuper},
	"grok-4.20-0309-super":               {tier: grokTierSuper},
	"grok-4.20-0309-reasoning-super":     {tier: grokTierSuper},
	"grok-4.20-0309-non-reasoning-heavy": {tier: grokTierHeavy},
	"grok-4.20-0309-heavy":               {tier: grokTierHeavy},
	"grok-4.20-0309-reasoning-heavy":     {tier: grokTierHeavy},
	"grok-4.20-multi-agent-0309":         {tier: grokTierHeavy},
	"grok-4.20-fast":                     {tier: grokTierBasic, preferBest: true},
	"grok-4.20-auto":                     {tier: grokTierSuper, preferBest: true},
	"grok-4.20-expert":                   {tier: grokTierSuper, preferBest: true},
	"grok-4.20-heavy":                    {tier: grokTierHeavy, preferBest: true},
	"grok-4.3-beta":                      {tier: grokTierSuper},
	"grok-imagine-image-lite":            {tier: grokTierBasic},
	"grok-imagine-image":                 {tier: grokTierSuper},
	"grok-imagine-image-pro":             {tier: grokTierSuper},
	"grok-imagine-image-edit":            {tier: grokTierSuper},
	"grok-imagine-video":                 {tier: grokTierSuper},
}

func IsPublicGrokModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	if id == "grok-4.3" {
		id = "grok-4.3-beta"
	}
	_, ok := publicGrokModelAllowlist[id]
	return ok
}

func IsStableGrokTextModelID(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	if id == "grok-4.3" {
		id = "grok-4.3-beta"
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

func GrokModelPoolCandidates(modelID string) []string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "grok-4.3" {
		id = "grok-4.3-beta"
	}
	routing, ok := grokModelRoutingByID[id]
	if !ok {
		return nil
	}
	switch {
	case routing.preferBest && routing.tier == grokTierHeavy:
		return []string{"heavy"}
	case routing.preferBest && routing.tier == grokTierSuper:
		return []string{"heavy", "super"}
	case routing.preferBest:
		return []string{"heavy", "super", "basic"}
	case routing.tier == grokTierHeavy:
		return []string{"heavy"}
	case routing.tier == grokTierSuper:
		return []string{"super", "heavy"}
	default:
		return []string{"basic", "super", "heavy"}
	}
}

func IsVisibleGrokModel(modelID string, verified bool) bool {
	return IsPublicGrokModelID(modelID) || verified
}
