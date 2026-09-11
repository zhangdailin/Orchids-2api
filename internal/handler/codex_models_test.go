package handler

import (
	"net/http"
	"net/http/httptest"
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
	return PublicModelResponse{ID: id, Object: "model", Capabilities: []string{"chat", "messages", "responses"}}
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

func TestCodexCatalogReasoningLevels(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		textModel("grok-4.5"),
		textModel("console/grok-4.20-0309-reasoning"),
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
	// The Console reasoning variant is fixed: it reasons but rejects the parameter.
	fixed := codexEntryFor(t, catalog, "console/grok-4.20-0309-reasoning")
	if len(fixed.SupportedReasoningLevels) != 0 || fixed.DefaultReasoningLevel != "none" {
		t.Fatalf("console fixed reasoning levels=%v default=%q", fixed.SupportedReasoningLevels, fixed.DefaultReasoningLevel)
	}
	if fixed.ContextWindow != 2000000 {
		t.Fatalf("console reasoning context=%d", fixed.ContextWindow)
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
