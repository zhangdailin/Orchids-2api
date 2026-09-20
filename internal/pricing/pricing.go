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

// ── Media pricing (images, videos) ────────────────────────────────────────────
//
// Ported from grok2api's EstimateOfficialImageCost / EstimateOfficialImageEditCost
// / EstimateOfficialVideoCost. The image and video planes are billed per produced
// asset rather than per token, so they get their own estimators instead of a
// token table.

const (
	officialImageEditInputTicks int64 = 100_000_000
	officialLiteImageInputTicks int64 = 20_000_000
)

// officialImage20OutputTicks is the resolution/quality matrix for 2.0.
func officialImage20OutputTicks(resolution, quality string) (int64, bool) {
	switch resolution + "/" + quality {
	case "1k/low":
		return 400_000_000, true
	case "2k/low", "1k/medium":
		return 600_000_000, true
	case "2k/medium":
		return 800_000_000, true
	default:
		return 0, false
	}
}

// EstimateImageCost prices a text-to-image request from the produced count.
func EstimateImageCost(model, resolution, quality string, count int) (Result, bool) {
	if count <= 0 {
		return Result{}, false
	}
	model = normalizePricingModel(model)
	quality = strings.ToLower(strings.TrimSpace(quality))
	switch model {
	case "grok-imagine-image":
		if quality != "" {
			return Result{}, false
		}
		return Result{Model: "grok-imagine-image", CostInUSDTicks: int64(count) * 200_000_000}, true
	case "grok-imagine-image-2.0":
		resolution = strings.ToLower(strings.TrimSpace(resolution))
		if resolution == "" {
			resolution = "1k"
		}
		if quality == "" {
			quality = "medium"
		}
		outputTicks, ok := officialImage20OutputTicks(resolution, quality)
		if !ok {
			return Result{}, false
		}
		return Result{
			Model:          "grok-imagine-image-2.0-" + quality + "-" + resolution,
			CostInUSDTicks: int64(count) * outputTicks,
		}, true
	case "grok-imagine-image-quality":
		if quality != "" {
			return Result{}, false
		}
		resolution = strings.ToLower(strings.TrimSpace(resolution))
		if resolution == "" {
			resolution = "1k"
		}
		var ticksPerImage int64
		switch resolution {
		case "1k":
			ticksPerImage = 500_000_000
		case "2k":
			ticksPerImage = 700_000_000
		default:
			return Result{}, false
		}
		return Result{
			Model:          "grok-imagine-image-quality-" + resolution,
			CostInUSDTicks: int64(count) * ticksPerImage,
		}, true
	default:
		return Result{}, false
	}
}

// EstimateImageEditCost prices an edit: every output image plus every input image
// the model had to process.
func EstimateImageEditCost(model, resolution, quality string, outputCount, inputCount int) (Result, bool) {
	model = normalizePricingModel(model)
	if outputCount <= 0 || inputCount <= 0 {
		return Result{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	quality = strings.ToLower(strings.TrimSpace(quality))
	if resolution == "" {
		resolution = "1k"
	}
	pricingModel := ""
	inputTicks := officialImageEditInputTicks
	var outputTicks int64
	switch model {
	case "grok-imagine-image-edit":
		if quality != "" {
			return Result{}, false
		}
		pricingModel = "grok-imagine-image-edit-" + resolution
	case "grok-imagine-image-2.0":
		if quality == "" {
			quality = "medium"
		}
		ticks, ok := officialImage20OutputTicks(resolution, quality)
		if !ok {
			return Result{}, false
		}
		outputTicks = ticks
		pricingModel = "grok-imagine-image-2.0-edit-" + quality + "-" + resolution
	case "grok-imagine-image-quality":
		if quality != "" {
			return Result{}, false
		}
		switch resolution {
		case "1k":
			outputTicks = 500_000_000
		case "2k":
			outputTicks = 700_000_000
		default:
			return Result{}, false
		}
		pricingModel = "grok-imagine-image-quality-" + resolution
	case "grok-imagine-image":
		if quality != "" {
			return Result{}, false
		}
		inputTicks = officialLiteImageInputTicks
		outputTicks = 200_000_000
		pricingModel = "grok-imagine-image"
	default:
		return Result{}, false
	}
	if outputTicks == 0 {
		switch resolution {
		case "1k":
			outputTicks = 500_000_000
		case "2k":
			outputTicks = 700_000_000
		default:
			return Result{}, false
		}
	}
	return Result{
		Model:          pricingModel,
		CostInUSDTicks: int64(outputCount)*outputTicks + int64(inputCount)*inputTicks,
	}, true
}

// EstimateVideoCost prices a generated video from its duration and resolution,
// plus the reference images the model consumed.
func EstimateVideoCost(model, resolution string, seconds, inputImages int) (Result, bool) {
	if seconds <= 0 || inputImages < 0 {
		return Result{}, false
	}
	baseModel := normalizePricingModel(model)
	if baseModel != "grok-imagine-video" && baseModel != "grok-imagine-video-1.5" {
		return Result{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	var ticksPerSecond, ticksPerInputImage int64
	switch baseModel {
	case "grok-imagine-video":
		ticksPerInputImage = officialLiteImageInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 500_000_000
		case "720p":
			ticksPerSecond = 700_000_000
		default:
			return Result{}, false
		}
	case "grok-imagine-video-1.5":
		ticksPerInputImage = officialImageEditInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 800_000_000
		case "720p":
			ticksPerSecond = 1_400_000_000
		case "1080p":
			ticksPerSecond = 2_500_000_000
		default:
			return Result{}, false
		}
	}
	return Result{
		Model:          baseModel + "-" + resolution,
		CostInUSDTicks: int64(seconds)*ticksPerSecond + int64(inputImages)*ticksPerInputImage,
	}, true
}

// ── Cost reconstruction (PricingBreakdown) ────────────────────────────────────
//
// grok2api exposes the rate components behind a stored cost so an operator can
// answer "why is this row this expensive" without re-deriving the formula. The
// stored row keeps the quantities (tokens, images, seconds) and the pricing
// model; this reconstructs the components that produced the number.

// ComponentKind names one line of a price.
type ComponentKind string

const (
	ComponentUncachedInput ComponentKind = "uncached_input"
	ComponentCachedInput   ComponentKind = "cached_input"
	ComponentOutput        ComponentKind = "output"
	ComponentInputImage    ComponentKind = "input_image"
	ComponentOutputImage   ComponentKind = "output_image"
	ComponentOutputSecond  ComponentKind = "output_second"
)

// Unit is what a component counts.
type Unit string

const (
	UnitToken  Unit = "token"
	UnitImage  Unit = "image"
	UnitSecond Unit = "second"
)

// Component is one priced line: quantity times unit price.
type Component struct {
	Kind                ComponentKind `json:"kind"`
	Unit                Unit          `json:"unit"`
	Quantity            int64         `json:"quantity"`
	UnitPriceInUSDTicks int64         `json:"unit_price_usd_ticks"`
	CostInUSDTicks      int64         `json:"cost_usd_ticks"`
}

// Breakdown is a reconstructed cost: the total plus the components that make it.
type Breakdown struct {
	Model          string      `json:"model"`
	CostInUSDTicks int64       `json:"cost_in_usd_ticks"`
	Components     []Component `json:"components"`
}

// addExact records a component whose exact cost is not quantity × unit price: the
// per-second rate of an hourly price is not an integer, so the rate is rounded
// for display while the cost stays the one the estimator charges.
func (b *Breakdown) addExact(kind ComponentKind, unit Unit, quantity, unitPrice, cost int64) {
	if quantity <= 0 || cost <= 0 {
		return
	}
	b.Components = append(b.Components, Component{
		Kind: kind, Unit: unit, Quantity: quantity, UnitPriceInUSDTicks: unitPrice, CostInUSDTicks: cost,
	})
	b.CostInUSDTicks += cost
}

func (b *Breakdown) add(kind ComponentKind, unit Unit, quantity, unitPrice int64) {
	if quantity <= 0 || unitPrice <= 0 {
		return
	}
	cost := quantity * unitPrice
	b.Components = append(b.Components, Component{
		Kind: kind, Unit: unit, Quantity: quantity, UnitPriceInUSDTicks: unitPrice, CostInUSDTicks: cost,
	})
	b.CostInUSDTicks += cost
}

// Quantities is what a stored row kept about its request.
type Quantities struct {
	InputTokens      int64
	CachedTokens     int64
	OutputTokens     int64
	ContextTokens    int64
	InputImages      int64
	OutputImages     int64
	OutputSeconds    int64
	Characters       int64
	StreamingSeconds float64
}

// ReconstructBreakdown rebuilds the components behind a priced row. It returns
// false when the model is not priced or its quantities are unusable, which is the
// same "unpriced" verdict the estimators give.
func ReconstructBreakdown(model string, q Quantities) (Breakdown, bool) {
	normalized := normalizePricingModel(model)
	breakdown := Breakdown{Model: normalized}

	// Media models first: their pricing model carries the tier suffix.
	if strings.HasPrefix(normalized, "grok-imagine-image") {
		return reconstructImageBreakdown(normalized, q)
	}
	if strings.HasPrefix(normalized, "grok-imagine-video") {
		// The stored name carries the resolution suffix (…-1.5-1080p), which the
		// reconstruction reads back out.
		return reconstructVideoBreakdown(normalized, q)
	}
	if normalized == "grok-voice-tts" {
		if q.Characters <= 0 {
			return Breakdown{}, false
		}
		breakdown.add(ComponentOutput, UnitSecond, q.Characters, officialTTSCharacterTicks)
		return breakdown, breakdown.CostInUSDTicks > 0
	}
	if strings.HasPrefix(normalized, "grok-stt-") {
		if q.StreamingSeconds <= 0 {
			return Breakdown{}, false
		}
		hourly := int64(1_000_000_000)
		if normalized == "grok-stt-streaming" {
			hourly = 2_000_000_000
		}
		cost := int64(math.Ceil(q.StreamingSeconds * float64(hourly) / 3600))
		if cost < 1 {
			cost = 1
		}
		breakdown.addExact(ComponentOutputSecond, UnitSecond, int64(math.Ceil(q.StreamingSeconds)), hourly/3600, cost)
		return breakdown, breakdown.CostInUSDTicks > 0
	}

	price, ok := resolveOfficialTokenPrice(normalized)
	if !ok {
		return Breakdown{}, false
	}
	contextTokens := q.ContextTokens
	if contextTokens <= 0 {
		contextTokens = q.InputTokens
	}
	inputPrice, cachedPrice, outputPrice := price.InputTicks, price.CachedInputTicks, price.OutputTicks
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice, cachedPrice, outputPrice = price.LongInputTicks, price.LongCachedTicks, price.LongOutputTicks
	}
	cached := max(int64(0), min(q.CachedTokens, q.InputTokens))
	uncached := max(int64(0), q.InputTokens-cached)
	breakdown.add(ComponentUncachedInput, UnitToken, uncached, inputPrice)
	breakdown.add(ComponentCachedInput, UnitToken, cached, cachedPrice)
	breakdown.add(ComponentOutput, UnitToken, max(int64(0), q.OutputTokens), outputPrice)
	return breakdown, breakdown.CostInUSDTicks > 0
}

func reconstructImageBreakdown(model string, q Quantities) (Breakdown, bool) {
	breakdown := Breakdown{Model: model}
	outputs := max(int64(0), q.OutputImages)
	inputs := max(int64(0), q.InputImages)
	switch {
	case model == "grok-imagine-image":
		breakdown.add(ComponentOutputImage, UnitImage, outputs, 200_000_000)
		breakdown.add(ComponentInputImage, UnitImage, inputs, officialLiteImageInputTicks)
	case strings.HasPrefix(model, "grok-imagine-image-2.0"):
		// The stored pricing model is "…-2.0-<quality>-<resolution>" (or with
		// "-edit" in the middle); recover both parts from the suffix.
		quality, resolution := "medium", "1k"
		rest := strings.TrimPrefix(model, "grok-imagine-image-2.0")
		rest = strings.TrimPrefix(rest, "-edit")
		rest = strings.TrimPrefix(rest, "-")
		if parts := strings.Split(rest, "-"); len(parts) == 2 {
			quality, resolution = parts[0], parts[1]
		}
		outputTicks, ok := officialImage20OutputTicks(resolution, quality)
		if !ok {
			return Breakdown{}, false
		}
		edit := strings.Contains(model, "-edit")
		breakdown.add(ComponentOutputImage, UnitImage, outputs, outputTicks)
		if edit {
			breakdown.add(ComponentInputImage, UnitImage, max(int64(1), inputs), officialImageEditInputTicks)
		}
	case strings.HasPrefix(model, "grok-imagine-image-quality"):
		resolution := "1k"
		if strings.HasSuffix(model, "-2k") {
			resolution = "2k"
		}
		outputTicks := int64(500_000_000)
		if resolution == "2k" {
			outputTicks = 700_000_000
		}
		breakdown.add(ComponentOutputImage, UnitImage, outputs, outputTicks)
		if strings.Contains(model, "edit") {
			breakdown.add(ComponentInputImage, UnitImage, max(int64(1), inputs), officialImageEditInputTicks)
		}
	case strings.Contains(model, "edit"):
		resolution := "1k"
		if strings.HasSuffix(model, "-2k") {
			resolution = "2k"
		}
		outputTicks := int64(500_000_000)
		if resolution == "2k" {
			outputTicks = 700_000_000
		}
		breakdown.add(ComponentOutputImage, UnitImage, outputs, outputTicks)
		breakdown.add(ComponentInputImage, UnitImage, max(int64(1), inputs), officialImageEditInputTicks)
	default:
		return Breakdown{}, false
	}
	return breakdown, breakdown.CostInUSDTicks > 0
}

func reconstructVideoBreakdown(model string, q Quantities) (Breakdown, bool) {
	if q.OutputSeconds <= 0 {
		return Breakdown{}, false
	}
	base := "grok-imagine-video"
	if strings.Contains(model, "1.5") {
		base = "grok-imagine-video-1.5"
	}
	resolution := "720p"
	switch {
	case strings.Contains(model, "480p"):
		resolution = "480p"
	case strings.Contains(model, "1080p"):
		resolution = "1080p"
	}
	var ticksPerSecond, ticksPerInputImage int64
	if base == "grok-imagine-video-1.5" {
		ticksPerInputImage = officialImageEditInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 800_000_000
		case "720p":
			ticksPerSecond = 1_400_000_000
		case "1080p":
			ticksPerSecond = 2_500_000_000
		default:
			return Breakdown{}, false
		}
	} else {
		ticksPerInputImage = officialLiteImageInputTicks
		switch resolution {
		case "480p":
			ticksPerSecond = 500_000_000
		case "720p":
			ticksPerSecond = 700_000_000
		default:
			return Breakdown{}, false
		}
	}
	breakdown := Breakdown{Model: base + "-" + resolution}
	breakdown.add(ComponentOutputSecond, UnitSecond, q.OutputSeconds, ticksPerSecond)
	breakdown.add(ComponentInputImage, UnitImage, q.InputImages, ticksPerInputImage)
	return breakdown, breakdown.CostInUSDTicks > 0
}
