package grok

import (
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/modelpolicy"
)

// UpstreamKind selects which Grok upstream protocol serves a model.
type UpstreamKind int

const (
	// UpstreamAuto derives the upstream from ModelSpec fields and config.
	UpstreamAuto UpstreamKind = iota
	// UpstreamCLI is cli-chat-proxy.grok.com/v1 + OAuth Bearer.
	UpstreamCLI
)

// ModelSpec defines one public model and how it maps to Grok upstream fields.
type ModelSpec struct {
	ID            string
	Name          string
	UpstreamModel string
	ModelMode     string
	ModeID        string
	ConsoleModel  string
	Tier          int
	PreferBest    bool
	IsImage       bool
	IsVideo       bool
	IsTTS         bool
	IsSTT         bool
	IsRealtime    bool
	MediaAPIOnly  bool
	// Upstream explicitly routes the model; UpstreamAuto derives from fields.
	Upstream UpstreamKind
	// AliasReasoningEffort is populated only while resolving an effort-suffixed
	// compatibility alias and is copied into the request before normalization.
	AliasReasoningEffort string
}

const (
	grokTierBasic = iota
	grokTierLite
	grokTierSuper
	grokTierHeavy
)

// SupportedModels is the deliberately small public compatibility table. Build
// OAuth capability snapshots remain authoritative; this table only describes the
// routes this gateway serves itself.
var SupportedModels = []ModelSpec{
	{ID: "grok-composer-2.5-fast", Name: "Grok Composer 2.5 Fast", UpstreamModel: "grok-composer-2.5-fast", Tier: grokTierSuper, Upstream: UpstreamCLI},
	{ID: "grok-4.5", Name: "Grok 4.5", UpstreamModel: "grok-4.5", Tier: grokTierSuper, Upstream: UpstreamCLI},
	{ID: "grok-4.6", Name: "Grok 4.6", UpstreamModel: "grok-4.6", Tier: grokTierSuper, Upstream: UpstreamCLI},
}

func (m ModelSpec) SupportsConversation() bool {
	return !m.IsTTS && !m.IsSTT && !m.IsRealtime && !m.MediaAPIOnly
}

var modelByID = func() map[string]ModelSpec {
	out := make(map[string]ModelSpec, len(SupportedModels))
	for _, m := range SupportedModels {
		out[strings.ToLower(strings.TrimSpace(m.ID))] = m
	}
	return out
}()

// providerCompatibilityAliases preserve the provider-qualified IDs while also
// accepting the unqualified and historical names published by grok2api.
var providerCompatibilityAliases = map[string]string{
	"grok-4.5-latest":  "grok-4.5",
	"grok-4.6-latest":  "grok-4.6",
	"grok-code-fast":   "grok-composer-2.5-fast",
	"grok-code-fast-1": "grok-composer-2.5-fast",
	"build/grok-4.5":   "grok-4.5",
	"build/grok-4.6":   "grok-4.6",
}

func IsDeprecatedModelID(modelID string) bool {
	return modelpolicy.IsDeprecatedGrokModelID(normalizeModelID(modelID))
}

func normalizeModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}

// stripProviderPublicPrefix removes one provider qualifier, reporting whether it
// removed anything.
func stripProviderPublicPrefix(id string) (string, bool) {
	return modelpolicy.StripProviderPublicPrefix(id)
}

// ParseReasoningModelAlias resolves a supported <model>-<effort> alias. The
// suffix is accepted only when the base model's provider contract advertises
// that exact effort, so names such as grok-4.5-xhigh remain model-not-found.
func ParseReasoningModelAlias(modelID string) (base, effort string, ok bool) {
	id := normalizeModelID(modelID)
	for _, candidate := range []string{"xhigh", "medium", "high", "low", "none"} {
		if !strings.HasSuffix(id, "-"+candidate) {
			continue
		}
		base = strings.TrimSuffix(id, "-"+candidate)
		canonical := base
		if alias, exists := providerCompatibilityAliases[base]; exists {
			canonical = alias
		}
		if modelpolicy.SupportsReasoningEffort(canonical, candidate) {
			return base, candidate, true
		}
	}
	return "", "", false
}

func ResolveModelAlias(modelID string) (ModelSpec, string, bool) {
	id := normalizeModelID(modelID)
	// Explicit provider-qualified IDs always win and remain supported.
	if m, exists := modelByID[id]; exists {
		return m, "", true
	}
	// Provider-qualified spellings in any casing. The exact table above has
	// already been consulted, so a qualifier that names a real route of another
	// plane is unaffected.
	if stripped, changed := stripProviderPublicPrefix(id); changed {
		if m, exists := modelByID[stripped]; exists {
			return m, "", true
		}
		if canonical, exists := providerCompatibilityAliases[stripped]; exists {
			m, found := modelByID[canonical]
			return m, "", found
		}
		id = stripped
	}
	if canonical, exists := providerCompatibilityAliases[id]; exists {
		m, found := modelByID[canonical]
		return m, "", found
	}
	if base, effort, exists := ParseReasoningModelAlias(id); exists {
		if canonical, aliased := providerCompatibilityAliases[base]; aliased {
			base = canonical
		}
		m, found := modelByID[base]
		return m, effort, found
	}
	return ModelSpec{}, "", false
}

func ResolveModel(modelID string) (ModelSpec, bool) {
	m, _, ok := ResolveModelAlias(modelID)
	return m, ok
}

func (m ModelSpec) PoolCandidates() []string {
	switch {
	case m.PreferBest && m.Tier == grokTierHeavy:
		return []string{"heavy", "basic"}
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

// modelRoutedToCLI reports whether a model should be served via the Build CLI
// upstream (explicit marker or config list).
func modelRoutedToCLI(spec ModelSpec, cfg *config.Config) bool {
	if spec.Upstream != UpstreamAuto {
		return spec.Upstream == UpstreamCLI
	}
	return cfg != nil && cfg.GrokModelIsCLI(spec.ID)
}
