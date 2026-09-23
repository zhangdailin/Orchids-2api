package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// Warp treats an absent base_model_context_window_limit as "use the model's
// default max". The builder used to pin it to a literal zero, which told the
// upstream nothing useful about a model that actually accepts a large window.
func TestBuildRequestSettingsStatesResolvedContextWindow(t *testing.T) {
	settings := buildRequestSettings(upstream.UpstreamRequest{WarpContextWindowLimit: 1000000}, false)
	modelConfig := settings.GetModelConfig()
	if !modelConfig.HasBaseModelContextWindowLimit() {
		t.Fatal("a resolved window must be sent, not dropped")
	}
	if got := modelConfig.GetBaseModelContextWindowLimit(); got != 1000000 {
		t.Fatalf("context window limit = %d, want 1000000", got)
	}
}

// Nothing observed means the field is absent, so Warp keeps the model's own max
// instead of being handed a zero.
func TestBuildRequestSettingsOmitsUnresolvedContextWindow(t *testing.T) {
	settings := buildRequestSettings(upstream.UpstreamRequest{}, false)
	if settings.GetModelConfig().HasBaseModelContextWindowLimit() {
		t.Fatalf("an unresolved window must stay absent, got %d",
			settings.GetModelConfig().GetBaseModelContextWindowLimit())
	}
}

func TestContextWindowsFromChoicesKeepsOnlyRealWindows(t *testing.T) {
	windows := ContextWindowsFromChoices([]ModelChoice{
		{ID: "gpt-5-6-sol-medium", ContextWindow: ModelContextWindow{Min: 1024, Max: 1000000, Default: 128000}},
		{ID: "no-window-declared"},
	})
	if len(windows) != 1 {
		t.Fatalf("windows = %#v, want only the choice that declared a max", windows)
	}
	if got := windows["gpt-5-6-sol-medium"].Max; got != 1000000 {
		t.Fatalf("max = %d, want 1000000", got)
	}
}

// One account reporting a smaller max must not shrink the window every other
// account can use.
func TestMergeContextWindowsKeepsTheLargestObservation(t *testing.T) {
	merged := MergeContextWindows(nil, map[string]ModelContextWindow{
		"model-a": {Max: 1000000},
	})
	merged = MergeContextWindows(merged, map[string]ModelContextWindow{
		"model-a": {Max: 200000},
		"model-b": {Max: 256000},
	})
	if got := merged["model-a"].Max; got != 1000000 {
		t.Fatalf("model-a max = %d, want the larger 1000000 kept", got)
	}
	if got := merged["model-b"].Max; got != 256000 {
		t.Fatalf("model-b max = %d, want 256000", got)
	}
}

// A client asks for the bare family name while discovery only published the
// effort variants; the window still has to resolve.
func TestModelContextWindowLimitForResolvesEffortVariant(t *testing.T) {
	choices := &AccountModelChoices{
		ContextWindows: map[string]ModelContextWindow{
			"gpt-5-6-sol-low":  {Max: 200000},
			"gpt-5-6-sol-high": {Max: 1000000},
		},
	}
	if got := ModelContextWindowLimitFor(choices, "gpt-5-6-sol"); got != 1000000 {
		t.Fatalf("family window = %d, want the largest variant 1000000", got)
	}
	if got := ModelContextWindowLimitFor(choices, "unknown-model"); got != 0 {
		t.Fatalf("unknown model window = %d, want 0 (absent)", got)
	}
	if got := ModelContextWindowLimitFor(nil, "gpt-5-6-sol"); got != 0 {
		t.Fatalf("no discovery window = %d, want 0 (absent)", got)
	}
}

// The stateless transcript ceiling is a transport bound, not a context policy.
// The historical 48 KiB replaced everything older than roughly 12k tokens.
func TestStatelessTranscriptKeepsWholeHistoryByDefault(t *testing.T) {
	SetStatelessHistoryMaxChars(0)
	defer SetStatelessHistoryMaxChars(0)
	if statelessHistoryMaxChars < 1<<20 {
		t.Fatalf("default ceiling = %d, want at least 1 MiB", statelessHistoryMaxChars)
	}

	// A transcript far beyond the old 48 KiB ceiling must survive intact.
	marker := "EARLIEST-TURN-MARKER"
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: marker}},
	}
	for i := 0; i < 400; i++ {
		messages = append(messages, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: strings.Repeat("x", 1024)}})
	}

	rendered := renderWarpStatelessTranscript(messages, nil)
	if !strings.Contains(rendered, marker) {
		t.Fatal("the earliest turn was dropped although the ceiling is above any model window")
	}
	if strings.Contains(rendered, "Earlier conversation omitted") {
		t.Fatal("history was truncated under the raised ceiling")
	}
}

func TestSetStatelessHistoryMaxCharsHonoursExplicitCeiling(t *testing.T) {
	SetStatelessHistoryMaxChars(1024)
	defer SetStatelessHistoryMaxChars(0)
	if statelessHistoryMaxChars != 1024 {
		t.Fatalf("ceiling = %d, want the configured 1024", statelessHistoryMaxChars)
	}
}
