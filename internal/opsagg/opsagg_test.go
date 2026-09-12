package opsagg

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedisClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func newAggregator(t *testing.T) (*Aggregator, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := newRedisClient(t, mini.Addr())
	return New(client, "ops-test:"), mini
}

// TestObserve_RollsUpIntoOneMinuteBucket covers the basic counters the overview
// renders.
func TestObserve_RollsUpIntoOneMinuteBucket(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now().Truncate(time.Minute)

	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: true, DurationMS: 1200, FirstTokenMS: 300, At: at})
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: true, DurationMS: 800, FirstTokenMS: 200, At: at})
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: false, DurationMS: 5000, At: at})
	// A probe must not be mixed into real traffic.
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: false, Synthetic: true, DurationMS: 10, At: at})

	buckets, err := agg.Range(ctx, "grok", at, at)
	if err != nil || len(buckets) != 1 {
		t.Fatalf("buckets = %v err = %v", buckets, err)
	}
	bucket := buckets[0]
	if bucket.Requests != 4 || bucket.Success != 2 || bucket.Failed != 2 || bucket.Probes != 1 {
		t.Fatalf("bucket = %+v", bucket)
	}

	summary := agg.Summarize(ctx, "grok", buckets)
	if summary.Requests != 4 || summary.Probes != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	// Real traffic is 3 requests, 2 of which succeeded.
	if got, want := summary.SuccessRate, 2.0/3.0; got < want-0.001 || got > want+0.001 {
		t.Fatalf("success rate = %v, want %v (probes excluded)", got, want)
	}
	if summary.DurationP95MS == 0 || summary.FirstTokenP95MS == 0 {
		t.Fatalf("percentiles missing: %+v", summary)
	}
	// The probe contributes a latency sample (it is a real round trip) but never
	// counts as a user request in the success ratio.
	if summary.Samples != 4 {
		t.Fatalf("samples = %d, want 4 latency observations", summary.Samples)
	}
}

// TestSummarize_NoSamplesIsZero is what makes the UI able to say "暂无样本"
// instead of drawing a green, zero-traffic channel.
func TestSummarize_NoSamplesIsZero(t *testing.T) {
	agg, _ := newAggregator(t)
	buckets, err := agg.Range(context.Background(), "puter", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("Range() error = %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("buckets = %v, want none for an idle channel", buckets)
	}
	summary := agg.Summarize(context.Background(), "puter", buckets)
	if summary.Requests != 0 || summary.Samples != 0 || summary.SuccessRate != 0 || summary.RPM != 0 {
		t.Fatalf("idle summary = %+v, want all zero", summary)
	}
}

// TestRange_SpansMinutesInOrder keeps the trend line in chronological order.
func TestRange_SpansMinutesInOrder(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	base := time.Now().Truncate(time.Minute).Add(-3 * time.Minute)

	for i := 0; i < 3; i++ {
		agg.Observe(ctx, Outcome{Channel: "warp", OK: true, DurationMS: 100, At: base.Add(time.Duration(i) * time.Minute)})
	}
	buckets, err := agg.Range(ctx, "warp", base, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Range() error = %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}
	for i := 1; i < len(buckets); i++ {
		if !buckets[i].Minute.After(buckets[i-1].Minute) {
			t.Fatalf("buckets are not in chronological order: %+v", buckets)
		}
	}
	if summary := agg.Summarize(ctx, "warp", buckets); summary.RPM <= 0 {
		t.Fatalf("rpm = %v, want > 0 over three minutes", summary.RPM)
	}
}

// TestModelStatsFromBuckets reports per-model quality without extra keys.
func TestModelStatsFromBuckets(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now().Truncate(time.Minute)

	for i := 0; i < 3; i++ {
		agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: i > 0, DurationMS: int64(100 * (i + 1)), At: at})
	}
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.5", OK: true, DurationMS: 50, At: at})

	buckets, err := agg.Range(ctx, "grok", at, at)
	if err != nil {
		t.Fatalf("Range() error = %v", err)
	}
	stats := agg.ModelStatsFromBuckets(ctx, "grok", buckets)
	if len(stats) != 2 {
		t.Fatalf("stats = %+v, want two models", stats)
	}
	// Sorted by request count: grok-4.6 leads.
	if stats[0].Model != "grok-4.6" || stats[0].Requests != 3 || stats[0].Success != 2 || stats[0].Failed != 1 {
		t.Fatalf("leading model = %+v", stats[0])
	}
}

// TestChannels_ListsOnlyObservedChannels lets the UI offer real options.
func TestChannels_ListsOnlyObservedChannels(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now()
	agg.Observe(ctx, Outcome{Channel: "grok", OK: true, At: at})
	agg.Observe(ctx, Outcome{Channel: "warp", OK: true, At: at})

	channels, err := agg.Channels(ctx, at, at)
	if err != nil {
		t.Fatalf("Channels() error = %v", err)
	}
	if len(channels) != 2 || channels[0] != "grok" || channels[1] != "warp" {
		t.Fatalf("channels = %v", channels)
	}
}

// TestPercentile_NearestRank pins the percentile definition used by the API.
func TestPercentile_NearestRank(t *testing.T) {
	values := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(values, 0.95); got != 100 {
		t.Fatalf("p95 = %d, want 100", got)
	}
	if got := percentile(values, 0.5); got != 60 {
		t.Fatalf("p50 = %d, want 60", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Fatalf("empty percentile = %d, want 0", got)
	}
}

// TestChannels_IgnoresSideLists pins the rule that a percentile list must never
// appear as a channel: the old code read the last colon-separated segment, so
// "...:dur" showed up as a channel named "dur".
func TestChannels_IgnoresSideLists(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now()
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: true, DurationMS: 120, FirstTokenMS: 40, At: at})

	channels, err := agg.Channels(ctx, at, at)
	if err != nil {
		t.Fatalf("Channels() error = %v", err)
	}
	for _, name := range channels {
		if name == "dur" || name == "ttft" || name == "grok-4.6" {
			t.Fatalf("side list leaked into the channel list: %v", channels)
		}
	}
	if len(channels) != 1 || channels[0] != "grok" {
		t.Fatalf("channels = %v, want just grok", channels)
	}
	// The percentile samples must survive for the summary to compute p95.
	buckets, err := agg.Range(ctx, "grok", at, at)
	if err != nil || len(buckets) != 1 {
		t.Fatalf("buckets = %v err = %v", buckets, err)
	}
	summary := agg.Summarize(ctx, "grok", buckets)
	if summary.Samples == 0 || summary.DurationP95MS != 120 {
		t.Fatalf("summary = %+v, want a 120ms p95 sample", summary)
	}
}
// TestSummarizeWith_RateUsesTheWindowNotTheBuckets is the reported RPM bug: a
// single request inside a sixty-minute window reported an RPM of 1, because the
// rate was divided by the number of buckets that happened to exist.
func TestSummarizeWith_RateUsesTheWindowNotTheBuckets(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now().Truncate(time.Minute)
	agg.Observe(ctx, Outcome{Channel: "grok", Model: "grok-4.6", OK: true, DurationMS: 100, At: at})

	buckets, err := agg.Range(ctx, "grok", at.Add(-59*time.Minute), at)
	if err != nil {
		t.Fatalf("Range() error = %v", err)
	}
	durations, ttfts := agg.SamplesFor(ctx, "grok", buckets)

	summary := agg.SummarizeWith(ctx, SummaryInput{
		Channel:         "grok",
		Buckets:         buckets,
		WindowMinutes:   60,
		Durations:       durations,
		FirstTokenMS:    ttfts,
		SamplesProvided: true,
	})
	if summary.Requests != 1 {
		t.Fatalf("requests = %d, want 1", summary.Requests)
	}
	if summary.RPM > 0.02 || summary.RPM < 0.01 {
		t.Fatalf("rpm = %v, want about 0.0167 for one request in an hour", summary.RPM)
	}
}

// TestSummarizeWith_SuccessRateNeverExceedsOne is the reported 200% bug: a probe
// recorded in the probe channel was subtracted from another scope's request count,
// so the ratio could exceed one.
func TestSummarizeWith_SuccessRateNeverExceedsOne(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now().Truncate(time.Minute)
	agg.Observe(ctx, Outcome{Channel: "grok", OK: true, DurationMS: 100, At: at})
	agg.Observe(ctx, Outcome{Channel: "probe", OK: true, DurationMS: 10, Synthetic: true, At: at})

	// Merging both channels is what the overview's "全部渠道" view does before the
	// infrastructure aggregates are filtered out.
	grok, _ := agg.Range(ctx, "grok", at, at)
	probe, _ := agg.Range(ctx, "probe", at, at)
	merged := append(append([]Bucket(nil), grok...), probe...)
	durations, ttfts := agg.SamplesFor(ctx, "grok", grok)
	probeDurations, probeTTFTs := agg.SamplesFor(ctx, "probe", probe)
	durations = append(durations, probeDurations...)
	ttfts = append(ttfts, probeTTFTs...)

	summary := agg.SummarizeWith(ctx, SummaryInput{
		Channel: "mixed", Buckets: merged, WindowMinutes: 60,
		Durations: durations, FirstTokenMS: ttfts, SamplesProvided: true,
	})
	if summary.SuccessRate > 1 {
		t.Fatalf("success rate = %v, want at most 1", summary.SuccessRate)
	}
	if summary.Requests != 2 {
		t.Fatalf("requests = %d, want both counted", summary.Requests)
	}
}

// TestSummarizeWith_MergedSamplesProducePercentiles is the reported P95 bug: the
// merged scope had no samples of its own, so both percentiles were flat zero while
// the trend showed traffic.
func TestSummarizeWith_MergedSamplesProducePercentiles(t *testing.T) {
	agg, _ := newAggregator(t)
	ctx := context.Background()
	at := time.Now().Truncate(time.Minute)
	for _, duration := range []int64{100, 200, 300} {
		agg.Observe(ctx, Outcome{Channel: "grok", OK: true, DurationMS: duration, FirstTokenMS: duration / 2, At: at})
	}
	for _, duration := range []int64{400, 500} {
		agg.Observe(ctx, Outcome{Channel: "warp", OK: true, DurationMS: duration, FirstTokenMS: duration / 2, At: at})
	}

	grokBuckets, _ := agg.Range(ctx, "grok", at, at)
	warpBuckets, _ := agg.Range(ctx, "warp", at, at)
	merged := append(append([]Bucket(nil), grokBuckets...), warpBuckets...)
	var durations, ttfts []int64
	for _, pair := range []struct {
		channel string
		buckets []Bucket
	}{{"grok", grokBuckets}, {"warp", warpBuckets}} {
		d, tt := agg.SamplesFor(ctx, pair.channel, pair.buckets)
		durations = append(durations, d...)
		ttfts = append(ttfts, tt...)
	}

	summary := agg.SummarizeWith(ctx, SummaryInput{
		Channel: "all", Buckets: merged, WindowMinutes: 60,
		Durations: durations, FirstTokenMS: ttfts, SamplesProvided: true,
	})
	if summary.Samples != 5 {
		t.Fatalf("samples = %d, want the five merged observations", summary.Samples)
	}
	if summary.DurationP95MS == 0 || summary.FirstTokenP95MS == 0 {
		t.Fatalf("merged percentiles are zero: %+v", summary)
	}
	// The merged p95 must reflect the slowest channel, not only the first one.
	if summary.DurationP95MS <= 300 {
		t.Fatalf("duration p95 = %d, want it to include the slower channel", summary.DurationP95MS)
	}
}