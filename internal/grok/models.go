package grok

import (
	"strings"
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
	{ID: "grok-4.3", Name: "Grok 4.3", UpstreamModel: "grok-4.3", ConsoleModel: "grok-4.3", Tier: grokTierSuper},
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

var deprecatedModelIDSet = map[string]struct{}{
	"grok-4.2":       {},
	"grok-4.20-beta": {},
	"grok-4.3-beta":  {},
	"grok-420":       {},
}

func IsDeprecatedModelID(modelID string) bool {
	id := normalizeModelID(modelID)
	if id == "" {
		return false
	}
	_, deprecated := deprecatedModelIDSet[id]
	return deprecated
}

func normalizeModelID(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	// Common typo compatibility: gork-* -> grok-*
	if strings.HasPrefix(m, "gork-") {
		m = "grok-" + strings.TrimPrefix(m, "gork-")
	}
	switch m {
	case "grok-imagine-1.0":
		return "grok-imagine-image"
	case "grok-imagine-1.0-fast":
		return "grok-imagine-image-lite"
	case "grok-imagine-1.0-edit":
		return "grok-imagine-image-edit"
	case "grok-imagine-1.0-video":
		return "grok-imagine-video"
	}
	// Version alias compatibility: grok-4-2 -> grok-4.2, grok-4-1-thinking -> grok-4.1-thinking.
	if strings.HasPrefix(m, "grok-") {
		rest := strings.TrimPrefix(m, "grok-")
		parts := strings.SplitN(rest, "-", 3)
		if len(parts) >= 2 && isDigits(parts[0]) && isDigits(parts[1]) {
			if len(parts) == 2 {
				return "grok-" + parts[0] + "." + parts[1]
			}
			return "grok-" + parts[0] + "." + parts[1] + "-" + parts[2]
		}
	}
	return m
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func ResolveModel(modelID string) (ModelSpec, bool) {
	m, ok := modelByID[normalizeModelID(modelID)]
	return m, ok
}

func resolveDynamicTextModel(modelID string) (ModelSpec, bool) {
	id := normalizeModelID(modelID)
	if id == "" {
		return ModelSpec{}, false
	}
	if IsDeprecatedModelID(id) {
		return ModelSpec{}, false
	}
	// Keep image/video models explicit; only auto-resolve text models.
	if strings.HasPrefix(id, "grok-imagine-") {
		return ModelSpec{}, false
	}
	if !strings.HasPrefix(id, "grok-") {
		return ModelSpec{}, false
	}
	return ModelSpec{
		ID:            id,
		Name:          id,
		UpstreamModel: id,
		ConsoleModel:  consoleModelForID(id),
	}, true
}

func consoleModelForID(id string) string {
	switch id {
	case "grok-4.3":
		return "grok-4.3"
	case "grok-build-0.1":
		return "grok-build-0.1"
	default:
		if strings.HasPrefix(id, "grok-") {
			return id
		}
		return ""
	}
}

func (m ModelSpec) PoolCandidates() []string {
	switch {
	case m.IsImage && normalizeModelID(m.ID) == "grok-imagine-image-lite":
		return []string{"basic", "lite", "super", "heavy"}
	case m.PreferBest && m.Tier == grokTierHeavy:
		return []string{"heavy"}
	case m.PreferBest && m.Tier == grokTierSuper:
		return []string{"heavy", "super", "lite"}
	case m.PreferBest && m.Tier == grokTierLite:
		return []string{"heavy", "super", "lite"}
	case m.PreferBest:
		return []string{"heavy", "super", "lite", "basic"}
	case m.Tier == grokTierHeavy:
		return []string{"heavy"}
	case m.Tier == grokTierSuper:
		return []string{"super", "lite", "heavy"}
	case m.Tier == grokTierLite:
		return []string{"lite", "super", "heavy"}
	default:
		return []string{"basic", "lite", "super", "heavy"}
	}
}

// ResolveModelOrDynamic first resolves built-in model specs, then falls back to
// dynamic text-model passthrough for newly introduced grok-* models.
func ResolveModelOrDynamic(modelID string) (ModelSpec, bool) {
	if spec, ok := ResolveModel(modelID); ok {
		return spec, true
	}
	return resolveDynamicTextModel(modelID)
}
