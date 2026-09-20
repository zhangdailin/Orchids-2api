package pricing

import (
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
		{"web prefix", "web/grok-4.5", "grok-4.5", 20000, 3000, 60000},
		{"console prefix", "console/grok-4.3", "grok-4.3", 12500, 2000, 25000},
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
		if Priced(model) {
			t.Fatalf("Priced(%q) = true, want false", model)
		}
	}
	if !Priced("build/grok-4.6") {
		t.Fatal("build/grok-4.6 must be priced")
	}
}

func TestEstimateTextReservation(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"grok-4.6","max_tokens":1000,"messages":[{"role":"user","content":"hello"}]}`)
	got, ok := EstimateTextReservation("grok-4.6", body)
	if !ok || got.Model != "grok-4.6" {
		t.Fatalf("reservation = %#v, %v", got, ok)
	}
	// The reservation must be at least the output cap priced at the output rate
	// and must not be smaller than settling the same request with no output.
	if want := int64(1000) * 60000; got.CostInUSDTicks < want {
		t.Fatalf("reservation %d < output-only floor %d", got.CostInUSDTicks, want)
	}
	settled, _ := EstimateCost("grok-4.6", 256, 0, 1000, 256)
	if got.CostInUSDTicks < settled.CostInUSDTicks {
		t.Fatalf("reservation %d < settled %d", got.CostInUSDTicks, settled.CostInUSDTicks)
	}

	// Unknown models and malformed bodies never reserve.
	if _, ok := EstimateTextReservation("gpt-5", body); ok {
		t.Fatal("unknown model must not reserve")
	}
	if _, ok := EstimateTextReservation("grok-4.6", nil); !ok {
		t.Fatal("an empty body still reserves the default output limit")
	}
	// A caller asking for more output reserves more.
	small, _ := EstimateTextReservation("grok-4.6", []byte(`{"max_tokens":100}`))
	large, _ := EstimateTextReservation("grok-4.6", []byte(`{"max_tokens":10000}`))
	if large.CostInUSDTicks <= small.CostInUSDTicks {
		t.Fatalf("large %d <= small %d", large.CostInUSDTicks, small.CostInUSDTicks)
	}
	// The output cap is clamped to the official maximum: asking for more than
	// 131072 output tokens reserves exactly as much as asking for 131072.
	huge, _ := EstimateTextReservation("grok-4.6", []byte(`{"max_tokens":99999999}`))
	capped, _ := EstimateTextReservation("grok-4.6", []byte(`{"max_tokens":131072}`))
	if huge.CostInUSDTicks != capped.CostInUSDTicks {
		t.Fatalf("clamp %d != %d", huge.CostInUSDTicks, capped.CostInUSDTicks)
	}
	if floor := int64(131_072) * 60000; huge.CostInUSDTicks < floor {
		t.Fatalf("clamped reservation %d < output floor %d", huge.CostInUSDTicks, floor)
	}
}

func TestEstimateTTSCost(t *testing.T) {
	t.Parallel()

	got, ok := EstimateTTSCost("héllo")
	if !ok || got.Model != "grok-voice-tts" || got.CostInUSDTicks != 5*150_000 {
		t.Fatalf("EstimateTTSCost = %#v, %v", got, ok)
	}
	if _, ok := EstimateTTSCost(""); ok {
		t.Fatal("empty text must not be priced")
	}
	if _, ok := EstimateTTSCost("   "); !ok {
		t.Fatal("whitespace is still billable characters")
	}
}

func TestEstimateSTTCost(t *testing.T) {
	t.Parallel()

	rest, ok := EstimateSTTCost(3600, false)
	if !ok || rest.Model != "grok-stt-rest" || rest.CostInUSDTicks != 1_000_000_000 {
		t.Fatalf("rest = %#v, %v", rest, ok)
	}
	stream, ok := EstimateSTTCost(3600, true)
	if !ok || stream.Model != "grok-stt-streaming" || stream.CostInUSDTicks != 2_000_000_000 {
		t.Fatalf("stream = %#v, %v", stream, ok)
	}
	// Sub-tick durations round up to one tick instead of disappearing.
	short, ok := EstimateSTTCost(0.000001, false)
	if !ok || short.CostInUSDTicks != 1 {
		t.Fatalf("short = %#v, %v", short, ok)
	}
	for _, invalid := range []float64{0, -1} {
		if _, ok := EstimateSTTCost(invalid, false); ok {
			t.Fatalf("duration %v must not be priced", invalid)
		}
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
