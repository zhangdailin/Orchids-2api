package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/goccy/go-json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/modelpolicy"
)

// Codex-family clients call GET /v1/models?client_version=... and expect a
// catalog richer than the OpenAI list: per-model context window, input
// modalities, reasoning levels and truncation policy. A client that only sees
// id/object/created/owned_by falls back to its own defaults, which is what makes
// a capable model look text-only with a tiny context window.

const codexBaseInstructions = "You are Codex, a coding agent. Follow the user's instructions and use the available tools to complete software engineering tasks. Inspect relevant files before editing, preserve unrelated changes, and verify the result."

type codexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

type codexTruncationPolicy struct {
	Mode  string `json:"mode"`
	Limit int    `json:"limit"`
}

type codexModelEntry struct {
	Slug                              string                `json:"slug"`
	DisplayName                       string                `json:"display_name"`
	Description                       string                `json:"description"`
	DefaultReasoningLevel             string                `json:"default_reasoning_level"`
	SupportedReasoningLevels          []codexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                         string                `json:"shell_type"`
	Visibility                        string                `json:"visibility"`
	MinimalClientVersion              string                `json:"minimal_client_version"`
	SupportedInAPI                    bool                  `json:"supported_in_api"`
	Priority                          int                   `json:"priority"`
	AdditionalSpeedTiers              []string              `json:"additional_speed_tiers"`
	ServiceTiers                      []any                 `json:"service_tiers"`
	BaseInstructions                  string                `json:"base_instructions"`
	IncludeSkillsUsageInstructions    bool                  `json:"include_skills_usage_instructions"`
	SupportsReasoningSummaryParameter bool                  `json:"supports_reasoning_summary_parameter"`
	SupportsReasoningSummaries        bool                  `json:"supports_reasoning_summaries"`
	DefaultReasoningSummary           string                `json:"default_reasoning_summary"`
	SupportVerbosity                  bool                  `json:"support_verbosity"`
	DefaultVerbosity                  *string               `json:"default_verbosity"`
	ApplyPatchToolType                *string               `json:"apply_patch_tool_type"`
	WebSearchToolType                 string                `json:"web_search_tool_type"`
	TruncationPolicy                  codexTruncationPolicy `json:"truncation_policy"`
	SupportsParallelToolCalls         bool                  `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal       bool                  `json:"supports_image_detail_original"`
	ContextWindow                     int                   `json:"context_window"`
	MaxContextWindow                  int                   `json:"max_context_window"`
	EffectiveContextWindowPercent     int                   `json:"effective_context_window_percent"`
	ExperimentalSupportedTools        []string              `json:"experimental_supported_tools"`
	InputModalities                   []string              `json:"input_modalities"`
	SupportsSearchTool                bool                  `json:"supports_search_tool"`
	UseResponsesLite                  bool                  `json:"use_responses_lite"`
}

type codexModelCatalog struct {
	Models []codexModelEntry `json:"models"`
}

var codexReasoningDescriptions = map[string]string{
	"none":   "No reasoning",
	"low":    "Fast responses with lighter reasoning",
	"medium": "Balances speed and reasoning depth for everyday tasks",
	"high":   "Greater reasoning depth for complex problems",
	"xhigh":  "Extra high reasoning depth for complex problems",
	"max":    "Maximum reasoning depth for the hardest problems",
}

type codexModelMetadata struct {
	contextWindow int
	description   string
	imageInput    bool
}

// Keyed by the upstream model slug, with provider prefixes already stripped.
var codexModelMetadataTable = map[string]codexModelMetadata{
	"grok-4.5":                     {500000, "xAI Grok 4.5 frontier model with reasoning and vision.", true},
	"grok-4.6":                     {500000, "xAI Grok 4.6 frontier model with reasoning and vision.", true},
	"grok-4.3":                     {1000000, "xAI Grok 4.3 high-capacity reasoning model.", true},
	"grok-build-0.1":               {256000, "xAI Grok Build 0.1 coding model.", false},
	"grok-4.20-0309-reasoning":     {2000000, "xAI Grok 4.20 reasoning model.", true},
	"grok-4.20-0309-non-reasoning": {2000000, "xAI Grok 4.20 non-reasoning model.", true},
	"grok-4.20-multi-agent-0309":   {2000000, "xAI Grok 4.20 multi-agent model.", true},
	"grok-3-mini":                  {131072, "xAI Grok 3 Mini model.", false},
	"grok-3-mini-fast":             {131072, "xAI Grok 3 Mini Fast model.", false},
	"grok-composer-2.5-fast":       {200000, "xAI Grok Composer 2.5 model.", false},
}

var codexDefaultMetadata = codexModelMetadata{
	contextWindow: 128000,
	description:   "Grok model served via this gateway.",
}

// Reasoning levels and provider scoping live in modelpolicy so the wire
// normalization and the advertised catalog can never diverge.
func codexReasoningLevelsFor(publicID string) []string {
	return modelpolicy.SupportedReasoningEfforts(publicID)
}

// The default level prefers medium, then the first supported level.
func codexDefaultReasoningLevel(levels []string) string {
	for _, level := range levels {
		if level == "medium" {
			return level
		}
	}
	if len(levels) > 0 {
		return levels[0]
	}
	return "none"
}

func codexReasoningLevelEntries(levels []string) []codexReasoningLevel {
	result := make([]codexReasoningLevel, 0, len(levels))
	for _, level := range levels {
		result = append(result, codexReasoningLevel{Effort: level, Description: codexReasoningDescriptions[level]})
	}
	return result
}

func codexHasCapability(item PublicModelResponse, capability string) bool {
	for _, value := range item.Capabilities {
		if strings.EqualFold(strings.TrimSpace(value), capability) {
			return true
		}
	}
	return false
}

// Media endpoints are not agent chat models, so they are listed but hidden.
func codexVisibilityFor(item PublicModelResponse) string {
	for _, capability := range []string{"image", "image_edit", "video"} {
		if codexHasCapability(item, capability) {
			return "hide"
		}
	}
	return "list"
}

func codexDisplayName(slug string) string {
	words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(slug))
	for index, word := range words {
		runes := []rune(word)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[index] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

// codexEffortSuffixes are the effort variants collapsed into one
// reasoning-capable catalog entry. Warp publishes gpt-5-6-sol-low, -medium,
// -high and -xhigh as separate models; presenting them as a single family with
// supported_reasoning_levels is what lets a client ask for the family plus an
// effort instead of guessing the exact suffix — and the gateway resolves that
// family name back onto the variant the catalog actually has.
var codexEffortSuffixes = []string{"low", "medium", "high", "xhigh", "max"}

// splitCodexEffortSuffix splits "<family>-<effort>" into its parts. Model ids
// without a known effort suffix are returned unchanged with an empty level.
func splitCodexEffortSuffix(modelID string) (family, level string) {
	for _, suffix := range codexEffortSuffixes {
		if strings.HasSuffix(modelID, "-"+suffix) {
			return strings.TrimSuffix(modelID, "-"+suffix), suffix
		}
	}
	return modelID, ""
}

func effortLevelRank(level string) int {
	for index, suffix := range codexEffortSuffixes {
		if suffix == level {
			return index
		}
	}
	return len(codexEffortSuffixes)
}

func sortEffortLevels(levels []string) {
	slices.SortStableFunc(levels, func(a, b string) int { return effortLevelRank(a) - effortLevelRank(b) })
}

func newCodexModelCatalog(items []PublicModelResponse) codexModelCatalog {
	type effortFamily struct {
		representative PublicModelResponse
		levels         []string
	}

	families := make(map[string]*effortFamily, len(items))
	order := make([]string, 0, len(items))
	for _, item := range items {
		name, level := splitCodexEffortSuffix(item.ID)
		entry, exists := families[name]
		if !exists {
			entry = &effortFamily{representative: item}
			families[name] = entry
			order = append(order, name)
		}
		if level != "" && !slices.Contains(entry.levels, level) {
			entry.levels = append(entry.levels, level)
		}
	}

	models := make([]codexModelEntry, 0, len(order))
	for index, name := range order {
		entry := families[name]
		item := entry.representative
		slug := modelpolicy.GrokModelSlug(name)
		metadata, ok := codexModelMetadataTable[slug]
		if !ok {
			metadata = codexDefaultMetadata
		}
		levels := entry.levels
		if len(levels) == 0 {
			levels = codexReasoningLevelsFor(name)
		} else {
			sortEffortLevels(levels)
		}
		modalities := []string{"text"}
		if metadata.imageInput {
			modalities = append(modalities, "image")
		}
		// Only text models served over the Responses API accept the agent toolset.
		toolsSupported := codexHasCapability(item, "responses")
		var applyPatchToolType *string
		if toolsSupported {
			value := "freeform"
			applyPatchToolType = &value
		}
		reasoningSupported := len(levels) > 0
		models = append(models, codexModelEntry{
			Slug:                              name,
			DisplayName:                       codexDisplayName(slug),
			Description:                       metadata.description,
			DefaultReasoningLevel:             codexDefaultReasoningLevel(levels),
			SupportedReasoningLevels:          codexReasoningLevelEntries(levels),
			ShellType:                         "shell_command",
			Visibility:                        codexVisibilityFor(item),
			MinimalClientVersion:              "0.0.0",
			SupportedInAPI:                    true,
			Priority:                          index + 1,
			AdditionalSpeedTiers:              []string{},
			ServiceTiers:                      []any{},
			BaseInstructions:                  codexBaseInstructions,
			IncludeSkillsUsageInstructions:    false,
			SupportsReasoningSummaryParameter: reasoningSupported,
			SupportsReasoningSummaries:        reasoningSupported,
			DefaultReasoningSummary:           "auto",
			SupportVerbosity:                  false,
			ApplyPatchToolType:                applyPatchToolType,
			WebSearchToolType:                 "text",
			TruncationPolicy:                  codexTruncationPolicy{Mode: "tokens", Limit: 10000},
			SupportsParallelToolCalls:         toolsSupported,
			SupportsImageDetailOriginal:       false,
			ContextWindow:                     metadata.contextWindow,
			MaxContextWindow:                  metadata.contextWindow,
			EffectiveContextWindowPercent:     95,
			ExperimentalSupportedTools:        []string{},
			InputModalities:                   modalities,
			SupportsSearchTool:                false,
			UseResponsesLite:                  false,
		})
	}
	return codexModelCatalog{Models: models}
}

func writeCodexModelCatalog(w http.ResponseWriter, r *http.Request, catalog codexModelCatalog) {
	body, err := json.Marshal(catalog)
	if err != nil {
		apperrors.New("api_error", "Failed to encode response", http.StatusInternalServerError).WriteResponse(w)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
