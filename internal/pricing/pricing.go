// Package pricing holds the official xAI price table and the pure functions the
// gateway uses to price a finished request and to reserve a budget before one
// starts. It is deliberately dependency-free: the same numbers must be usable
// from the request path, the audit writer and tests.
//
// Money is carried as integer USD ticks (1 USD = 10,000,000,000 ticks) so a
// ledger can accumulate costs without floating point drift. The published rates
// are per 1M tokens, which is 1e6 times the per-token tick value used here.
package pricing

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// Source is the official page the table below was transcribed from.
	Source = "https://docs.x.ai/developers/pricing"
	// AsOf is the date the published rates were captured.
	AsOf = "2026-08-13"
	// Version identifies the table revision. It is persisted on every priced
	// ledger/audit row so an old row can always be read against the rates that
	// produced it.
	Version = "official-2026-08-13"
	// TicksPerUSD is the fixed point scale of every cost in this package.
	TicksPerUSD int64 = 10_000_000_000
)

// Result is one priced request: the canonical model the rate was resolved to and
// its cost in USD ticks. An empty Result with ok == false means "unpriced",
// which callers must keep distinguishable from a genuine zero cost.
type Result struct {
	Model          string
	CostInUSDTicks int64
}

// officialTTSCharacterTicks is xAI's $15 per 1M characters, per character.
const officialTTSCharacterTicks int64 = 150_000

// tokenPrice is one row of the official text table. The *Ticks values are
// integer USD ticks per token; LongContextTokens is the input size above which
// the long-context column applies.
type tokenPrice struct {
	CanonicalModel    string
	InputTicks        int64
	CachedInputTicks  int64
	OutputTicks       int64
	LongContextTokens int64
	LongInputTicks    int64
	LongCachedTicks   int64
	LongOutputTicks   int64
}

var officialTokenPrices = buildOfficialTokenPrices()

// tokenPriceRule resolves a model family when the exact name is not published:
// the official page prices every suffix of a family at the family rate.
type tokenPriceRule struct {
	Pattern        *regexp.Regexp
	CanonicalModel string
}

// officialTokenPriceRules keeps the two special shapes (non-reasoning and
// multi-agent) ahead of the generic family pattern, so a specific suffix is
// never priced as the reasoning default.
var officialTokenPriceRules = []tokenPriceRule{
	{Pattern: regexp.MustCompile(`^grok-(?:build-0\.1|code-fast(?:-1)?|composer-2\.5-fast)(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-build-0.1"},
	{Pattern: regexp.MustCompile(`^grok-4\.6(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.6"},
	{Pattern: regexp.MustCompile(`^grok-4\.5(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.5"},
	{Pattern: regexp.MustCompile(`^grok-4\.3(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.3"},
	{Pattern: regexp.MustCompile(`^grok-4\.20-multi-agent(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-multi-agent-0309"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*-non-reasoning(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-non-reasoning"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-reasoning"},
}

// buildOfficialTokenPrices returns the official per-token rate table. Every
// alias resolves to the same canonical row.
//
// Published rates (USD per 1M tokens, standard / long-context above 200k input):
//
//	grok-build-0.1              $1     / $0.20 cached / $2     -> $2 / $0.40 / $4
//	grok-4.6                    $2     / $0.50 cached / $6     -> $4 / $1    / $12
//	grok-4.5                    $2     / $0.30 cached / $6     -> $4 / $0.60 / $12
//	grok-4.3 and 4.20 family    $1.25  / $0.20 cached / $2.50  -> $2.50 / $0.40 / $5
func buildOfficialTokenPrices() map[string]tokenPrice {
	prices := make(map[string]tokenPrice)
	register := func(canonical string, price tokenPrice, names ...string) {
		price.CanonicalModel = canonical
		for _, name := range append([]string{canonical}, names...) {
			prices[name] = price
		}
	}
	register("grok-build-0.1", tokenPrice{InputTicks: 10000, CachedInputTicks: 2000, OutputTicks: 20000, LongContextTokens: 200000, LongInputTicks: 20000, LongCachedTicks: 4000, LongOutputTicks: 40000},
		"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825", "grok-composer-2.5-fast")
	register("grok-4.6", tokenPrice{InputTicks: 20000, CachedInputTicks: 5000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 10000, LongOutputTicks: 120000},
		"grok-4.6-latest")
	register("grok-4.5", tokenPrice{InputTicks: 20000, CachedInputTicks: 3000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 6000, LongOutputTicks: 120000},
		"grok-4.5-latest", "grok-build-latest")
	standard := tokenPrice{InputTicks: 12500, CachedInputTicks: 2000, OutputTicks: 25000, LongContextTokens: 200000, LongInputTicks: 25000, LongCachedTicks: 4000, LongOutputTicks: 50000}
	register("grok-4.3", standard, "grok-4.3-latest", "grok-latest")
	register("grok-4.20-multi-agent-0309", standard,
		"grok-4.20-multi-agent", "grok-4.20-multi-agent-latest", "grok-4.20-multi-agent-beta-latest", "grok-4.20-multi-agent-beta-0309")
	register("grok-4.20-0309-reasoning", standard,
		"grok-4.20-reasoning-latest", "grok-4.20", "grok-4.20-reasoning", "grok-4.20-0309", "grok-4.20-beta", "grok-4.20-beta-0309", "grok-4.20-beta-latest", "grok-4.20-beta-reasoning", "grok-4.20-beta-latest-reasoning")
	register("grok-4.20-0309-non-reasoning", standard,
		"grok-4.20-non-reasoning", "grok-4.20-non-reasoning-latest", "grok-4.20-beta-non-reasoning", "grok-4.20-beta-latest-non-reasoning")
	return prices
}

// Priced reports whether a model name resolves to an official rate. An unpriced
// model must never be treated as costing zero.
func Priced(model string) bool {
	_, ok := resolveOfficialTokenPrice(model)
	return ok
}

// resolveOfficialTokenPrice handles the internal source prefixes and the exact
// published aliases first, then the anchored family rules.
func resolveOfficialTokenPrice(model string) (tokenPrice, bool) {
	normalized := normalizePricingModel(model)
	if price, ok := officialTokenPrices[normalized]; ok {
		return price, true
	}
	for _, rule := range officialTokenPriceRules {
		if !rule.Pattern.MatchString(normalized) {
			continue
		}
		price, ok := officialTokenPrices[rule.CanonicalModel]
		return price, ok
	}
	return tokenPrice{}, false
}

// normalizePricingModel strips only the source prefixes the gateway itself
// attaches, so an arbitrary path fragment is never mistaken for a billable model.
func normalizePricingModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"build/", "web/", "console/", "grok_build/", "grok_web/", "grok_console/"} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimSpace(normalized[len(prefix):])
		}
	}
	return normalized
}

// EstimateCost prices one finished text request from its token usage.
//
// cachedInputTokens is the cached part of inputTokens (clamped to [0, input]);
// contextInputTokens selects the long-context column and defaults to inputTokens
// when zero, which is the best available approximation when the caller only
// knows the billed input size. Unknown models return false and a zero Result.
func EstimateCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (Result, bool) {
	price, ok := resolveOfficialTokenPrice(model)
	if !ok {
		return Result{}, false
	}
	inputPrice := price.InputTicks
	cachedPrice := price.CachedInputTicks
	outputPrice := price.OutputTicks
	contextTokens := contextInputTokens
	if contextTokens <= 0 {
		contextTokens = inputTokens
	}
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice = price.LongInputTicks
		cachedPrice = price.LongCachedTicks
		outputPrice = price.LongOutputTicks
	}
	inputTokens = max(int64(0), inputTokens)
	cachedTokens := max(int64(0), min(cachedInputTokens, inputTokens))
	uncachedTokens := max(int64(0), inputTokens-cachedTokens)
	outputTokens = max(int64(0), outputTokens)
	return Result{
		Model:          price.CanonicalModel,
		CostInUSDTicks: uncachedTokens*inputPrice + cachedTokens*cachedPrice + outputTokens*outputPrice,
	}, true
}

// EstimateTextReservation prices the worst case of a request that has not run
// yet: the tokens observed in the body plus the output limit the caller asked
// for. It is intentionally conservative — an over-reservation is released when
// the request settles.
func EstimateTextReservation(model string, body []byte) (Result, bool) {
	if _, ok := resolveOfficialTokenPrice(model); !ok {
		return Result{}, false
	}
	inputTokens := estimateRequestInputTokens(body)
	outputTokens := estimateRequestOutputLimit(body)
	return EstimateCost(model, inputTokens, 0, outputTokens, inputTokens)
}

// EstimateTTSCost prices unary TTS from the exact Unicode character count that
// was accepted by the upstream request ($15 per 1M characters).
func EstimateTTSCost(text string) (Result, bool) {
	characters := utf8.RuneCountInString(text)
	if characters <= 0 {
		return Result{}, false
	}
	return Result{
		Model:          "grok-voice-tts",
		CostInUSDTicks: int64(characters) * officialTTSCharacterTicks,
	}, true
}

// EstimateSTTCost prices a completed STT request from the duration reported by
// the upstream: REST costs $0.10/hour and streaming costs $0.20/hour.
func EstimateSTTCost(durationSeconds float64, streaming bool) (Result, bool) {
	if durationSeconds <= 0 || math.IsNaN(durationSeconds) || math.IsInf(durationSeconds, 0) {
		return Result{}, false
	}
	hourlyTicks := int64(1_000_000_000)
	model := "grok-stt-rest"
	if streaming {
		hourlyTicks = 2_000_000_000
		model = "grok-stt-streaming"
	}
	// Round upward to one USD tick so a positive billable duration never
	// disappears through integer truncation.
	cost := int64(math.Ceil(durationSeconds * float64(hourlyTicks) / 3600))
	return Result{Model: model, CostInUSDTicks: max(int64(1), cost)}, true
}

// estimateRequestOutputLimit reads the caller's output cap. A request without
// one is assumed to be allowed the gateway default, which is what makes the
// reservation an upper bound rather than a guess.
func estimateRequestOutputLimit(body []byte) int64 {
	const defaultOutputTokens int64 = 16_384
	const maximumOutputTokens int64 = 131_072
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return defaultOutputTokens
	}
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		var value int64
		if raw, ok := payload[key]; ok && json.Unmarshal(raw, &value) == nil && value > 0 {
			return min(value, maximumOutputTokens)
		}
	}
	return defaultOutputTokens
}

// estimateRequestInputTokens approximates prompt tokens from the request JSON.
// JSON structure and keys are counted as text, matching the upstream
// implementation's estimate so the two reserve comparable amounts.
func estimateRequestInputTokens(body []byte) int64 {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return max(256, int64((len(body)+2)/3))
	}
	return max(256, estimateJSONTokens(payload)+128)
}

func estimateJSONTokens(value any) int64 {
	switch typed := value.(type) {
	case map[string]any:
		var total int64
		for key, child := range typed {
			total += int64((len(key)+2)/3) + 1 + estimateJSONTokens(child)
		}
		return total
	case []any:
		var total int64
		for _, child := range typed {
			total += 1 + estimateJSONTokens(child)
		}
		return total
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "data:image/") || strings.HasPrefix(trimmed, "data:video/") {
			return 256
		}
		return max(1, int64((len(typed)+2)/3))
	case json.Number, float64, bool:
		return 1
	case nil:
		return 0
	default:
		encoded, _ := json.Marshal(typed)
		return max(1, int64((len(encoded)+2)/3))
	}
}
