package grok

import (
	"strings"

	"orchids-api/internal/modelpolicy"
)

// ModelSpec defines one public model and how it maps to Grok upstream fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
	ModelMode     string
	ConsoleModel  string
	Tier          int
	PreferBest    bool
	IsImage       bool
	IsVideo       bool
}

const (
	grokTierBasic = iota
	grokTierLite
	grokTierSuper
	grokTierHeavy
)

// SupportedModels is the Go-native model table ported from grok2api behavior.
var SupportedModels = []ModelSpec{
	{ID: "grok-4.20-0309-non-reasoning", Name: "Grok 4.20 0309 Non-Reasoning", UpstreamModel: "grok-4.20-0309-non-reasoning", ModelMode: "MODEL_MODE_FAST", Tier: grokTierBasic},
	{ID: "grok-4.20-0309", Name: "Grok 4.20 0309", UpstreamModel: "grok-4.20-0309", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper},
	{ID: "grok-4.20-0309-reasoning", Name: "Grok 4.20 0309 Reasoning", UpstreamModel: "grok-4.20-0309-reasoning", ModelMode: "MODEL_MODE_EXPERT", Tier: grokTierSuper},
	{ID: "grok-4.20-0309-non-reasoning-super", Name: "Grok 4.20 0309 Non-Reasoning Super", UpstreamModel: "grok-4.20-0309-non-reasoning-super", ModelMode: "MODEL_MODE_FAST", Tier: grokTierSuper},
	{ID: "grok-4.20-0309-super", Name: "Grok 4.20 0309 Super", UpstreamModel: "grok-4.20-0309-super", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper},
	{ID: "grok-4.20-0309-reasoning-super", Name: "Grok 4.20 0309 Reasoning Super", UpstreamModel: "grok-4.20-0309-reasoning-super", ModelMode: "MODEL_MODE_EXPERT", Tier: grokTierSuper},
	{ID: "grok-4.20-0309-non-reasoning-heavy", Name: "Grok 4.20 0309 Non-Reasoning Heavy", UpstreamModel: "grok-4.20-0309-non-reasoning-heavy", ModelMode: "MODEL_MODE_FAST", Tier: grokTierHeavy},
	{ID: "grok-4.20-0309-heavy", Name: "Grok 4.20 0309 Heavy", UpstreamModel: "grok-4.20-0309-heavy", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierHeavy},
	{ID: "grok-4.20-0309-reasoning-heavy", Name: "Grok 4.20 0309 Reasoning Heavy", UpstreamModel: "grok-4.20-0309-reasoning-heavy", ModelMode: "MODEL_MODE_EXPERT", Tier: grokTierHeavy},
	{ID: "grok-4.20-multi-agent-0309", Name: "Grok 4.20 Multi-Agent 0309", UpstreamModel: "grok-4.20-multi-agent-0309", ModelMode: "MODEL_MODE_HEAVY", Tier: grokTierHeavy},
	{ID: "grok-4.20-fast", Name: "Grok 4.20 Fast", UpstreamModel: "grok-4.20-fast", ModelMode: "MODEL_MODE_FAST", Tier: grokTierBasic, PreferBest: true},
	{ID: "grok-4.20-auto", Name: "Grok 4.20 Auto", UpstreamModel: "grok-4.20-auto", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper, PreferBest: true},
	{ID: "grok-4.20-expert", Name: "Grok 4.20 Expert", UpstreamModel: "grok-4.20-expert", ModelMode: "MODEL_MODE_EXPERT", Tier: grokTierSuper, PreferBest: true},
	{ID: "grok-4.20-heavy", Name: "Grok 4.20 Heavy", UpstreamModel: "grok-4.20-heavy", ModelMode: "MODEL_MODE_HEAVY", Tier: grokTierHeavy, PreferBest: true},
	{ID: "grok-4.3", Name: "Grok 4.3", UpstreamModel: "grok-4.3-beta", ModelMode: "grok-420-computer-use-sa", Tier: grokTierSuper},
	{ID: "grok-build-0.1", Name: "Grok Build 0.1", UpstreamModel: "grok-build-0.1", ConsoleModel: "grok-build-0.1", Tier: grokTierSuper},
	{ID: "grok-imagine-image-lite", Name: "Grok Imagine Image Lite", UpstreamModel: "grok-imagine-image-lite", ModelMode: "MODEL_MODE_FAST", Tier: grokTierBasic, IsImage: true},
	{ID: "grok-imagine-image", Name: "Grok Imagine Image", UpstreamModel: "grok-imagine-image", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-image-pro", Name: "Grok Imagine Image Pro", UpstreamModel: "grok-imagine-image-pro", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-image-edit", Name: "Grok Imagine Image Edit", UpstreamModel: "imagine-image-edit", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-video", Name: "Grok Imagine Video", UpstreamModel: "imagine-video-gen", ModelMode: "MODEL_MODE_AUTO", Tier: grokTierSuper, IsVideo: true},
}

var modelByID = func() map[string]ModelSpec {
	out := make(map[string]ModelSpec, len(SupportedModels))
	for _, m := range SupportedModels {
		out[strings.ToLower(strings.TrimSpace(m.ID))] = m
	}
	return out
}()

func IsDeprecatedModelID(modelID string) bool {
	return modelpolicy.IsDeprecatedGrokModelID(normalizeModelID(modelID))
}

func normalizeModelID(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	return m
}

func ResolveModel(modelID string) (ModelSpec, bool) {
	m, ok := modelByID[normalizeModelID(modelID)]
	return m, ok
}

func (m ModelSpec) PoolCandidates() []string {
	switch {
	case m.IsImage && normalizeModelID(m.ID) == "grok-imagine-image-lite" && m.Tier == grokTierBasic:
		return []string{"lite", "super", "heavy"}
	case m.PreferBest && m.Tier == grokTierHeavy:
		return []string{"heavy", "basic"}
	case m.PreferBest && m.Tier == grokTierSuper:
		return []string{"heavy", "super", "lite", "basic"}
	case m.PreferBest && m.Tier == grokTierLite:
		return []string{"heavy", "super", "lite", "basic"}
	case m.PreferBest:
		return []string{"heavy", "super", "lite", "basic"}
	case m.Tier == grokTierHeavy:
		return []string{"heavy", "basic"}
	case m.Tier == grokTierSuper:
		return []string{"super", "lite", "heavy", "basic"}
	case m.Tier == grokTierLite:
		return []string{"lite", "super", "heavy", "basic"}
	default:
		return []string{"basic", "lite", "super", "heavy"}
	}
}

func ResolveModelOrDynamic(modelID string) (ModelSpec, bool) {
	return ResolveModel(modelID)
}
