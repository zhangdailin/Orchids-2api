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
	// UpstreamAppChat is the grok.com/rest/app-chat/... website protocol.
	UpstreamAppChat
	// UpstreamConsole is console.x.ai/v1/responses + DPoP.
	UpstreamConsole
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
// OAuth capability snapshots remain authoritative for text models; this table
// only describes currently supported routes, not a historical model archive.
var SupportedModels = []ModelSpec{
	{ID: "grok-composer-2.5-fast", Name: "Grok Composer 2.5 Fast", UpstreamModel: "grok-composer-2.5-fast", Tier: grokTierSuper, Upstream: UpstreamCLI},
	{ID: "grok-4.5", Name: "Grok 4.5", UpstreamModel: "grok-4.5", Tier: grokTierSuper, Upstream: UpstreamCLI},
	{ID: "grok-4.6", Name: "Grok 4.6", UpstreamModel: "grok-4.6", Tier: grokTierSuper, Upstream: UpstreamCLI},
	// Grok Web chat products use the app-chat protocol and Web SSO accounts.
	{ID: "grok-chat-fast", Name: "Grok Chat Fast", UpstreamModel: "grok-chat-fast", ModelMode: "MODEL_MODE_FAST", ModeID: "fast", Tier: grokTierBasic, Upstream: UpstreamAppChat},
	{ID: "grok-chat-auto", Name: "Grok Chat Auto", UpstreamModel: "grok-chat-auto", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, Upstream: UpstreamAppChat},
	{ID: "grok-chat-expert", Name: "Grok Chat Expert", UpstreamModel: "grok-chat-expert", ModelMode: "MODEL_MODE_EXPERT", ModeID: "expert", Tier: grokTierSuper, Upstream: UpstreamAppChat},
	{ID: "grok-chat-heavy", Name: "Grok Chat Heavy", UpstreamModel: "grok-chat-heavy", ModelMode: "MODEL_MODE_HEAVY", ModeID: "heavy", Tier: grokTierHeavy, PreferBest: true, Upstream: UpstreamAppChat},
	// Console routes are provider-qualified where their public name would collide
	// with Build. Keeping the provider in the ID makes routing deterministic.
	{ID: "console/grok-4.3", Name: "Console Grok 4.3", ConsoleModel: "grok-4.3", Tier: grokTierSuper, Upstream: UpstreamConsole},
	{ID: "console/grok-4.20-0309-reasoning", Name: "Console Grok 4.20 Reasoning", ConsoleModel: "grok-4.20-0309-reasoning", Tier: grokTierSuper, Upstream: UpstreamConsole},
	{ID: "console/grok-4.20-0309-non-reasoning", Name: "Console Grok 4.20 Non-Reasoning", ConsoleModel: "grok-4.20-0309-non-reasoning", Tier: grokTierSuper, Upstream: UpstreamConsole},
	{ID: "console/grok-4.20-multi-agent-0309", Name: "Console Grok 4.20 Multi-Agent", ConsoleModel: "grok-4.20-multi-agent-0309", Tier: grokTierHeavy, PreferBest: true, Upstream: UpstreamConsole},
	{ID: "console/grok-4.5", Name: "Console Grok 4.5", ConsoleModel: "grok-4.5", Tier: grokTierSuper, Upstream: UpstreamConsole},
	{ID: "console/grok-build-0.1", Name: "Console Grok Build 0.1", ConsoleModel: "grok-build-0.1", Tier: grokTierSuper, Upstream: UpstreamConsole},
	{ID: "console/grok-imagine-image", Name: "Console Grok Imagine Image", ConsoleModel: "grok-imagine-image", Tier: grokTierBasic, IsImage: true, MediaAPIOnly: true, Upstream: UpstreamConsole},
	{ID: "console/grok-imagine-image-quality", Name: "Console Grok Imagine Image Quality", ConsoleModel: "grok-imagine-image-quality", Tier: grokTierBasic, IsImage: true, MediaAPIOnly: true, Upstream: UpstreamConsole},
	{ID: "console/grok-imagine-image-2.0", Name: "Console Grok Imagine Image 2.0", ConsoleModel: "grok-imagine-image-2.0", Tier: grokTierBasic, IsImage: true, MediaAPIOnly: true, Upstream: UpstreamConsole},
	{ID: "grok-imagine-image-lite", Name: "Grok Imagine Image Lite", UpstreamModel: "grok-imagine-image-lite", ModelMode: "MODEL_MODE_FAST", ModeID: "fast", Tier: grokTierBasic, IsImage: true},
	{ID: "grok-imagine-image", Name: "Grok Imagine Image", UpstreamModel: "grok-imagine-image", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-image-2.0", Name: "Grok Imagine Image 2.0", UpstreamModel: "grok-imagine-image-2.0", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	// grok-imagine-image-quality is a Console media product in grok2api, not a Web
	// Imagine one: pointing the public name at the Web `-lite` upstream made the
	// same model name reach a different plane (and a different billing basis)
	// than the reference implementation.
	{ID: "grok-imagine-image-quality", Name: "Grok Imagine Image Quality", UpstreamModel: "grok-imagine-image-quality", ConsoleModel: "grok-imagine-image-quality", Tier: grokTierBasic, IsImage: true, MediaAPIOnly: true, Upstream: UpstreamConsole},
	// grok-imagine-image-pro is deprecated: it is unconditionally rejected by
	// IsDeprecatedModelID, so advertising it only produced a catalog entry that
	// every request failed on. The pro route is grok-imagine-image-2.0.
	{ID: "grok-imagine-image-edit", Name: "Grok Imagine Image Edit", UpstreamModel: "imagine-image-edit", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsImage: true},
	{ID: "grok-imagine-video", Name: "Grok Imagine Video", UpstreamModel: "imagine-video-gen", ModelMode: "MODEL_MODE_AUTO", ModeID: "auto", Tier: grokTierSuper, IsVideo: true},
	{ID: "grok-imagine-video-1.5", Name: "Grok Imagine Video 1.5", UpstreamModel: "grok-imagine-video-1.5", Upstream: UpstreamConsole, IsVideo: true, MediaAPIOnly: true},
	{ID: "build/grok-imagine-video-1.5", Name: "Build Grok Imagine Video 1.5", UpstreamModel: "grok-imagine-video-1.5", Upstream: UpstreamCLI, IsVideo: true, MediaAPIOnly: true},
	{ID: "grok-voice-latest", Name: "Grok Voice Latest", UpstreamModel: "grok-voice-latest", Upstream: UpstreamConsole, IsTTS: true, IsRealtime: true},
	{ID: "grok-voice-think-fast-2.0", Name: "Grok Voice Think Fast 2.0", UpstreamModel: "grok-voice-think-fast-2.0", Upstream: UpstreamConsole, IsTTS: true, IsRealtime: true},
	{ID: "grok-voice-think-fast-1.0", Name: "Grok Voice Think Fast 1.0", UpstreamModel: "grok-voice-think-fast-1.0", Upstream: UpstreamConsole, IsTTS: true, IsRealtime: true},
	{ID: "grok-stt", Name: "Grok Speech to Text", UpstreamModel: "grok-stt", Upstream: UpstreamConsole, IsSTT: true},
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
	"grok-4.3":                             "console/grok-4.3",
	"grok-4.3-console":                     "console/grok-4.3",
	"grok-4.20-0309-reasoning":             "console/grok-4.20-0309-reasoning",
	"grok-4.20-0309-reasoning-console":     "console/grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning":         "console/grok-4.20-0309-non-reasoning",
	"grok-4.20-0309-non-reasoning-console": "console/grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent-0309":           "console/grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-console":        "console/grok-4.20-multi-agent-0309",
	"grok-4.5-console":                     "console/grok-4.5",
	"grok-build-0.1":                       "console/grok-build-0.1",
	"grok-build-console":                   "console/grok-build-0.1",
	"grok-imagine-image-quality-2.0":       "console/grok-imagine-image-quality",
	// The remaining registered names grok2api accepts (console/catalog.go), where
	// a route is reached through a provider-qualified or effort-baked alias.
	"grok-4.6-console":               "console/grok-4.5",
	"grok-4.5-latest":                "grok-4.5",
	"grok-4.6-latest":                "grok-4.6",
	"grok-4.3-latest":                "console/grok-4.3",
	"grok-4.20":                      "console/grok-4.20-0309-reasoning",
	"grok-4.20-reasoning":            "console/grok-4.20-0309-reasoning",
	"grok-4.20-non-reasoning":        "console/grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent":          "console/grok-4.20-multi-agent-0309",
	"grok-4.20-multi-agent-beta":     "console/grok-4.20-multi-agent-0309",
	"grok-4.20-beta":                 "console/grok-4.20-0309-reasoning",
	"grok-4.20-beta-reasoning":       "console/grok-4.20-0309-reasoning",
	"grok-4.20-beta-non-reasoning":   "console/grok-4.20-0309-non-reasoning",
	"grok-code-fast":                 "grok-composer-2.5-fast",
	"grok-code-fast-1":               "grok-composer-2.5-fast",
	"build/grok-build-0.1":           "console/grok-build-0.1",
	"build/grok-4.5":                 "grok-4.5",
	"console/grok-4.6":               "grok-4.6",
	"build/grok-4.6":                 "grok-4.6",
	"console/grok-imagine-video":     "grok-imagine-video",
	"console/grok-imagine-video-1.5": "grok-imagine-video-1.5",
	"build/grok-imagine-video":       "grok-imagine-video",
	"web/grok-imagine-video":         "grok-imagine-video",
	"web/grok-imagine-video-1.5":     "grok-imagine-video-1.5",
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

func ConsoleFallbackFor(spec ModelSpec) (ModelSpec, bool) {
	if spec.Upstream != UpstreamCLI {
		return ModelSpec{}, false
	}
	slug := strings.ToLower(strings.TrimSpace(spec.UpstreamModel))
	fallback, ok := modelByID["console/"+slug]
	return fallback, ok
}

func (m ModelSpec) PoolCandidates() []string {
	switch {
	case m.IsImage && normalizeModelID(m.ID) == "grok-imagine-image-lite" && m.Tier == grokTierBasic:
		// Basic is this model's minimum tier, not an exclusion: a deployment
		// whose only credentials are free/basic Web accounts can serve it. The
		// model's own tier is still preferred, so lite comes first.
		return []string{"lite", "basic", "super", "heavy"}
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
