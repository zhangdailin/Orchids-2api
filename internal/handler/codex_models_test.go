package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func codexEntryFor(t *testing.T, catalog codexModelCatalog, slug string) codexModelEntry {
	t.Helper()
	for _, entry := range catalog.Models {
		if entry.Slug == slug {
			return entry
		}
	}
	t.Fatalf("catalog is missing %q", slug)
	return codexModelEntry{}
}

func textModel(id string) PublicModelResponse {
	return PublicModelResponse{ID: id, Object: "model", Provider: "build", Capabilities: []string{"chat", "messages", "responses"}}
}

// A client that cannot see the context window falls back to its own default,
// which is what makes a capable model look like an 8k-context one.
func TestCodexCatalogExposesContextWindowAndModalities(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		textModel("grok-build-0.1"),
		textModel("grok-unknown-model"),
	})

	for _, tc := range []struct {
		slug       string
		context    int
		modalities []string
	}{
		{"grok-4.6", 500000, []string{"text", "image"}},
		{"grok-build-0.1", 256000, []string{"text"}},
		{"grok-unknown-model", 128000, []string{"text"}},
	} {
		entry := codexEntryFor(t, catalog, tc.slug)
		if entry.ContextWindow != tc.context || entry.MaxContextWindow != tc.context {
			t.Fatalf("%s context_window=%d max=%d want %d", tc.slug, entry.ContextWindow, entry.MaxContextWindow, tc.context)
		}
		if len(entry.InputModalities) != len(tc.modalities) {
			t.Fatalf("%s input_modalities=%v want %v", tc.slug, entry.InputModalities, tc.modalities)
		}
		for i, want := range tc.modalities {
			if entry.InputModalities[i] != want {
				t.Fatalf("%s input_modalities=%v want %v", tc.slug, entry.InputModalities, tc.modalities)
			}
		}
	}
}

func TestCodexCatalogUsesObservedGrokProfile(t *testing.T) {
	supportsReasoning := true
	observed := textModel("grok-4.7")
	observed.ReasoningEfforts = []string{"low", "high", "xhigh"}
	observed.DefaultReasoningEffort = "high"
	observed.SupportsReasoningEffort = &supportsReasoning
	observed.ContextLength = 500000
	observed.MaxInputTokens = 500000
	observed.MaxOutputTokens = 1000000

	entry := codexEntryFor(t, newCodexModelCatalog([]PublicModelResponse{observed}), "grok-4.7")
	levels := make([]string, 0, len(entry.SupportedReasoningLevels))
	for _, level := range entry.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	if strings.Join(levels, ",") != "low,high,xhigh" {
		t.Fatalf("observed reasoning levels = %v, want low,high,xhigh", levels)
	}
	if entry.DefaultReasoningLevel != "high" {
		t.Fatalf("observed default reasoning level = %q, want high", entry.DefaultReasoningLevel)
	}
	if entry.ContextWindow != 500000 || entry.MaxContextWindow != 1500000 {
		t.Fatalf("observed windows = context %d max %d, want 500000 and 1500000", entry.ContextWindow, entry.MaxContextWindow)
	}
	// The declared output budget travels with the catalog so a client can plan
	// its own completion against the same number the gateway advertises.
	if entry.MaxOutputTokens != 1000000 {
		t.Fatalf("observed max output tokens = %d, want 1000000", entry.MaxOutputTokens)
	}
}

func TestCodexCatalogReasoningLevels(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		textModel("grok-4.5"),
	})

	entry := codexEntryFor(t, catalog, "grok-4.6")
	if len(entry.SupportedReasoningLevels) != 4 || entry.DefaultReasoningLevel != "medium" {
		t.Fatalf("grok-4.6 levels=%v default=%q", entry.SupportedReasoningLevels, entry.DefaultReasoningLevel)
	}
	if entry.SupportedReasoningLevels[3].Effort != "xhigh" || entry.SupportedReasoningLevels[3].Description == "" {
		t.Fatalf("grok-4.6 top level = %#v", entry.SupportedReasoningLevels[3])
	}
	// Grok 4.5 tops out at high.
	if levels := codexEntryFor(t, catalog, "grok-4.5").SupportedReasoningLevels; len(levels) != 3 || levels[2].Effort != "high" {
		t.Fatalf("grok-4.5 levels=%v", levels)
	}
}

func TestCodexCatalogHidesMediaModels(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		{ID: "grok-imagine-image", Capabilities: []string{"image", "image_edit"}},
		{ID: "grok-imagine-video", Capabilities: []string{"video"}},
	})
	if got := codexEntryFor(t, catalog, "grok-4.6").Visibility; got != "list" {
		t.Fatalf("text model visibility=%q", got)
	}
	for _, slug := range []string{"grok-imagine-image", "grok-imagine-video"} {
		if got := codexEntryFor(t, catalog, slug).Visibility; got != "hide" {
			t.Fatalf("%s visibility=%q want hide", slug, got)
		}
	}
	// Agent tooling is only advertised for Responses-capable text models.
	if entry := codexEntryFor(t, catalog, "grok-4.6"); entry.ApplyPatchToolType == nil || !entry.SupportsParallelToolCalls {
		t.Fatalf("grok-4.6 tooling = %#v", entry)
	}
	if entry := codexEntryFor(t, catalog, "grok-imagine-image"); entry.ApplyPatchToolType != nil {
		t.Fatalf("media model advertised apply_patch")
	}
}

func TestCodexCatalogJSONIncludesNullableProtocolFields(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{textModel("grok-4.6")})
	rec := httptest.NewRecorder()
	writeCodexModelCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=1", nil), catalog)
	body := rec.Body.String()
	for _, field := range []string{"default_service_tier", "availability_nux", "upgrade", "model_messages", "auto_compact_token_limit"} {
		if !strings.Contains(body, `"`+field+`":null`) {
			t.Fatalf("missing nullable field %q in %s", field, body)
		}
	}
}

func TestWriteCodexModelCatalogServesETag(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{textModel("grok-4.6")})

	first := httptest.NewRecorder()
	writeCodexModelCatalog(first, httptest.NewRequest(http.MethodGet, "/v1/models", nil), catalog)
	if first.Code != http.StatusOK {
		t.Fatalf("status=%d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	revalidate := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	revalidate.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	writeCodexModelCatalog(second, revalidate, catalog)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("revalidate status=%d body=%q", second.Code, second.Body.String())
	}
}

// A channel may publish one model per effort level (gpt-5-6-sol-low, -medium,
// ...). The catalog must present that as one reasoning-capable family, otherwise
// a client has to guess a suffix and any family request is rejected.
func TestCodexCatalogGroupsEffortVariantsIntoOneFamily(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("gpt-5-6-sol-low"),
		textModel("gpt-5-6-sol-medium"),
		textModel("gpt-5-6-sol-high"),
		textModel("gpt-5-6-sol-xhigh"),
		textModel("auto-open"),
	})

	if len(catalog.Models) != 2 {
		t.Fatalf("catalog has %d entries, want the family plus auto-open", len(catalog.Models))
	}
	family := codexEntryFor(t, catalog, "gpt-5-6-sol")
	levels := make([]string, 0, len(family.SupportedReasoningLevels))
	for _, level := range family.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	if strings.Join(levels, ",") != "low,medium,high,xhigh" {
		t.Fatalf("supported levels = %v, want the catalog's effort variants", levels)
	}
	if family.DefaultReasoningLevel != "medium" {
		t.Fatalf("default level = %q, want medium", family.DefaultReasoningLevel)
	}
	if !family.SupportsReasoningSummaries || !family.SupportsReasoningSummaryParameter {
		t.Fatalf("a family with effort levels must advertise reasoning support: %+v", family)
	}

	plain := codexEntryFor(t, catalog, "auto-open")
	// Models without effort variants keep the historical single "none" level.
	if len(plain.SupportedReasoningLevels) != 1 || plain.SupportedReasoningLevels[0].Effort != "none" {
		t.Fatalf("auto-open levels = %+v, want the default none level", plain.SupportedReasoningLevels)
	}
	if plain.DefaultReasoningLevel != "none" {
		t.Fatalf("auto-open default level = %q, want none", plain.DefaultReasoningLevel)
	}
}

func TestSplitEffortVariantSuffix(t *testing.T) {
	cases := map[string][2]string{
		"gpt-5-6-sol-low":        {"gpt-5-6-sol", "low"},
		"gpt-5-3-codex-xhigh":    {"gpt-5-3-codex", "xhigh"},
		"grok-4.6":               {"grok-4.6", ""},
		"auto-open":              {"auto-open", ""},
		"grok-composer-2.5-fast": {"grok-composer-2.5-fast", ""},
	}
	for input, want := range cases {
		family, level := splitEffortVariantSuffix(input)
		if family != want[0] || level != want[1] {
			t.Fatalf("splitEffortVariantSuffix(%q) = (%q, %q), want (%q, %q)", input, family, level, want[0], want[1])
		}
	}
}

// A lone "<family>-<effort>" id is not a family. Collapsing it would advertise a
// slug the store does not have while hiding the one it does, so a client that
// trusted the catalog would ask for a model that cannot be routed.
func TestCodexCatalogKeepsASingleEffortVariantUnderItsOwnID(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("gpt-5-6-sol-high"),
		textModel("grok-4.6"),
	})

	if len(catalog.Models) != 2 {
		t.Fatalf("catalog has %d entries, want the single variant plus grok-4.6", len(catalog.Models))
	}
	entry := codexEntryFor(t, catalog, "gpt-5-6-sol-high")
	if entry.DefaultReasoningLevel != "none" {
		t.Fatalf("single variant default level = %q, want none", entry.DefaultReasoningLevel)
	}
	if len(entry.SupportedReasoningLevels) != 1 || entry.SupportedReasoningLevels[0].Effort != "none" {
		t.Fatalf("single variant levels = %+v, want the default none level", entry.SupportedReasoningLevels)
	}
}

// The family representative drives visibility, capabilities and metadata. A
// hidden variant must not mask a family that has a visible one.
func TestCodexCatalogPrefersAVisibleVariantsMetadata(t *testing.T) {
	hidden := textModel("gpt-5-6-sol-low")
	hidden.Capabilities = []string{"chat", "messages", "responses", "image"}
	visible := textModel("gpt-5-6-sol-high")

	catalog := newCodexModelCatalog([]PublicModelResponse{
		hidden,
		visible,
	})

	family := codexEntryFor(t, catalog, "gpt-5-6-sol")
	if family.Visibility != "list" {
		t.Fatalf("family visibility = %q, want list from the visible variant", family.Visibility)
	}
	levels := make([]string, 0, len(family.SupportedReasoningLevels))
	for _, level := range family.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	if strings.Join(levels, ",") != "low,high" {
		t.Fatalf("supported levels = %v, want both variants' efforts", levels)
	}
}
