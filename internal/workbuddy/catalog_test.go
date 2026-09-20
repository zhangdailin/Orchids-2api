package workbuddy

import "testing"

// The snapshot used to be a bare list of ids, which preserved the whitelist but
// discarded the windows it was observed alongside. A client that budgets its
// context then had nothing to read.
func TestCatalogSnapshotRoundTripsWindows(t *testing.T) {
	models := []WorkBuddyModel{
		{ID: "wb-model", Name: "WB Model", MaxInputTokens: 256000, MaxOutputTokens: 32000},
		{ID: "wb-small", Name: "WB Small", MaxInputTokens: 128000},
	}
	rows := CatalogSnapshot(models)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	input, output := CatalogContextWindows(rows)
	if got := input["wb-model"]; got != 256000 {
		t.Fatalf("wb-model input window = %d, want 256000", got)
	}
	if got := output["wb-model"]; got != 32000 {
		t.Fatalf("wb-model output budget = %d, want 32000", got)
	}
	if got := input["wb-small"]; got != 128000 {
		t.Fatalf("wb-small input window = %d, want 128000", got)
	}
	if got := output["wb-small"]; got != 0 {
		t.Fatalf("wb-small output budget = %d, want absent", got)
	}
}

// A snapshot written by an older build is a bare id. It still has to resolve;
// there is simply no window to recover from it.
func TestCatalogContextWindowsAcceptsLegacyBareIDs(t *testing.T) {
	input, output := CatalogContextWindows([]string{"legacy-model", `{"id":"new-model","max_input_tokens":1000000}`})
	if _, ok := input["legacy-model"]; ok {
		t.Fatalf("a bare id must not invent a window: %#v", input)
	}
	if got := input["new-model"]; got != 1000000 {
		t.Fatalf("new-model window = %d, want 1000000", got)
	}
	if output != nil {
		t.Fatalf("output budgets = %#v, want nil when none were declared", output)
	}
}

func TestCatalogContextWindowsIgnoresMalformedRows(t *testing.T) {
	input, _ := CatalogContextWindows([]string{"{not json", "", "   "})
	if input != nil {
		t.Fatalf("input windows = %#v, want nil", input)
	}
}
