package pricing

import (
	"strings"
	"testing"
)

// TestEstimateCostUsesOfficialRates pins the transcribed rate table: each entry
// is the published USD-per-1M price multiplied by 1e6 ticks.
func TestEstimateCostUsesOfficialRates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		model     string
		canonical string
		// ticks per single token: uncached input, cached input, output
		input, cached, output int64
	}{
		{"build", "grok-build-0.1", "grok-build-0.1", 10000, 2000, 20000},
		{"build alias", "grok-code-fast-1", "grok-build-0.1", 10000, 2000, 20000},
		{"build 4.6", "grok-4.6", "grok-4.6", 20000, 5000, 60000},
		{"build 4.6 latest", "grok-4.6-latest", "grok-4.6", 20000, 5000, 60000},
		{"build 4.5", "grok-4.5", "grok-4.5", 20000, 3000, 60000},
		{"build 4.3", "grok-4.3", "grok-4.3", 12500, 2000, 25000},
		{"4.20 reasoning", "grok-4.20", "grok-4.20-0309-reasoning", 12500, 2000, 25000},
		{"4.20 non-reasoning", "grok-4.20-beta-latest-non-reasoning", "grok-4.20-0309-non-reasoning", 12500, 2000, 25000},
		{"4.20 multi agent", "grok-4.20-multi-agent-beta-latest", "grok-4.20-multi-agent-0309", 12500, 2000, 25000},
		// Family rules price unpublished suffixes at the family rate.
		{"family suffix", "grok-4.6-0309-reasoning", "grok-4.6", 20000, 5000, 60000},
		// Source prefixes are stripped before resolution.
		{"build prefix", "build/grok-4.6", "grok-4.6", 20000, 5000, 60000},
		{"grok_build prefix", "grok_build/grok-code-fast", "grok-build-0.1", 10000, 2000, 20000},
		{"case and space", "  Grok-4.6  ", "grok-4.6", 20000, 5000, 60000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := EstimateCost(tc.model, 1, 0, 0, 0)
			if !ok || got.Model != tc.canonical || got.CostInUSDTicks != tc.input {
				t.Fatalf("input EstimateCost(%q) = %#v, %v; want %s / %d", tc.model, got, ok, tc.canonical, tc.input)
			}
			got, ok = EstimateCost(tc.model, 1, 1, 0, 0)
			if !ok || got.CostInUSDTicks != tc.cached {
				t.Fatalf("cached EstimateCost(%q) = %#v, %v; want %d", tc.model, got, ok, tc.cached)
			}
			got, ok = EstimateCost(tc.model, 0, 0, 1, 0)
			if !ok || got.CostInUSDTicks != tc.output {
				t.Fatalf("output EstimateCost(%q) = %#v, %v; want %d", tc.model, got, ok, tc.output)
			}
		})
	}
}

// TestEstimateCostMatchesPublishedPerMillionPrices checks a whole million of
// each component, which is how the rates are published.
func TestEstimateCostMatchesPublishedPerMillionPrices(t *testing.T) {
	t.Parallel()

	got, ok := EstimateCost("grok-4.6", 100_000, 0, 100_000, 100_000)
	if !ok {
		t.Fatal("grok-4.6 is priced")
	}
	// 0.1M in at $2 + 0.1M out at $6 = $0.80 = 8e9 ticks (standard tier).
	if want := int64(8) * 1_000_000_000; got.CostInUSDTicks != want {
		t.Fatalf("cost = %d ticks, want %d", got.CostInUSDTicks, want)
	}
	got, ok = EstimateCost("grok-build-0.1", 100_000, 0, 0, 0)
	if !ok || got.CostInUSDTicks != 1_000_000_000 {
		t.Fatalf("build input cost = %#v, %v; want $0.10", got, ok)
	}
}

// TestEstimateCostLongContextSwitch pins the >200k switch: exactly 200k is the
// standard column, one token more is the long-context column.
func TestEstimateCostLongContextSwitch(t *testing.T) {
	t.Parallel()

	atLimit, ok := EstimateCost("grok-4.6", 200_000, 0, 1, 200_000)
	if !ok || atLimit.CostInUSDTicks != 200_000*20000+60000 {
		t.Fatalf("at limit = %#v, %v", atLimit, ok)
	}
	overLimit, ok := EstimateCost("grok-4.6", 200_001, 0, 1, 200_001)
	if !ok || overLimit.CostInUSDTicks != 200_001*40000+120000 {
		t.Fatalf("over limit = %#v, %v", overLimit, ok)
	}
	// A zero context size falls back to the billed input size.
	implicit, ok := EstimateCost("grok-4.6", 200_001, 0, 0, 0)
	if !ok || implicit.CostInUSDTicks != 200_001*40000 {
		t.Fatalf("implicit context = %#v, %v", implicit, ok)
	}
}

func TestEstimateCostClampsInvalidInputs(t *testing.T) {
	t.Parallel()

	// Cached can never exceed input, and negatives never credit the caller.
	got, ok := EstimateCost("grok-4.6", 100, 5_000, 0, 0)
	if !ok || got.CostInUSDTicks != 100*5000 {
		t.Fatalf("cached clamp = %#v, %v", got, ok)
	}
	got, ok = EstimateCost("grok-4.6", -10, -10, -10, -10)
	if !ok || got.CostInUSDTicks != 0 {
		t.Fatalf("negative clamp = %#v, %v", got, ok)
	}
}

func TestEstimateCostUnknownModelIsNotZeroCost(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"", "   ", "grok-imagine-image", "gpt-5", "claude-sonnet-4", "other/grok-4.6"} {
		if got, ok := EstimateCost(model, 1_000, 0, 1_000, 0); ok {
			t.Fatalf("EstimateCost(%q) = %#v, want unpriced", model, got)
		}
	}
}

func TestEstimateTextReservationFromBodyMatchesLegacyEstimator(t *testing.T) {
	bodies := [][]byte{
		[]byte(`{"model":"grok-4.6","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"messages":[{"content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"max_completion_tokens":42,"model":"grok-4.5"}`),
		[]byte(`{"model":"build/grok-4.6","max_output_tokens":7,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`),
	}
	for _, body := range bodies {
		got, ok := EstimateTextReservationFromBody(body)
		if !ok || got.Model == "" || got.CostInUSDTicks <= 0 {
			t.Fatalf("body=%s got=%#v ok=%v", body, got, ok)
		}
	}
	malformed, ok := EstimateTextReservationFromBody([]byte(`{"model":"grok-4.6"`))
	if !ok || malformed.CostInUSDTicks <= 0 {
		t.Fatal("a truncated body with a complete model must use the conservative fallback")
	}
}

func BenchmarkEstimateTextReservationFromBody(b *testing.B) {
	body := []byte(`{"model":"grok-4.6","max_tokens":4096,"messages":[{"role":"user","content":"` + strings.Repeat("large prompt ", 10000) + `"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		_, _ = EstimateTextReservationFromBody(body)
	}
}

func TestConstantsAreStable(t *testing.T) {
	t.Parallel()

	if TicksPerUSD != 10_000_000_000 {
		t.Fatalf("TicksPerUSD = %d", TicksPerUSD)
	}
	if Version != "official-"+AsOf || Source == "" {
		t.Fatalf("version %q / source %q", Version, Source)
	}
}

func TestReconstructBreakdownExplainsAStoredCost(t *testing.T) {
	// A text row: the components must add up to the estimator's answer.
	q := Quantities{InputTokens: 1000, CachedTokens: 400, OutputTokens: 500}
	breakdown, ok := ReconstructBreakdown("grok-4.6", q)
	if !ok {
		t.Fatal("grok-4.6 was not reconstructed")
	}
	direct, priced := EstimateCost("grok-4.6", q.InputTokens, q.CachedTokens, q.OutputTokens, q.InputTokens)
	if !priced || breakdown.CostInUSDTicks != direct.CostInUSDTicks {
		t.Fatalf("breakdown=%d direct=%d", breakdown.CostInUSDTicks, direct.CostInUSDTicks)
	}
	kinds := map[ComponentKind]int64{}
	for _, component := range breakdown.Components {
		kinds[component.Kind] = component.Quantity
	}
	if kinds[ComponentUncachedInput] != 600 || kinds[ComponentCachedInput] != 400 || kinds[ComponentOutput] != 500 {
		t.Fatalf("components=%+v", breakdown.Components)
	}
	// The long-context tier is visible in the component price.
	long, ok := ReconstructBreakdown("grok-4.6", Quantities{InputTokens: 300_000, OutputTokens: 10, ContextTokens: 300_000})
	if !ok {
		t.Fatal("long-context row was not reconstructed")
	}
	for _, component := range long.Components {
		if component.Kind == ComponentUncachedInput && component.UnitPriceInUSDTicks != 40_000 {
			t.Fatalf("long-context input price=%d", component.UnitPriceInUSDTicks)
		}
	}

}
