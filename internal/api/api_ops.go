package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/redis/go-redis/v9"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/store"
)

// defaultOpsWindowMinutes is the window the overview opens with.
const defaultOpsWindowMinutes = 180

// nonProviderChannels are aggregates that are counted but must not be presented
// as provider channels in the matrix or the channel picker:
//
//   - "http" is every request whose path matched no provider prefix: the admin
//     UI, health checks, and whatever a public scanner asks for. It has no
//     accounts and no models, so it cannot be "healthy" or "unhealthy".
//   - "probe" is our own synthetic traffic, recorded with the reserved model
//     "__probe__" so it can never be confused with a real request. It is shown as
//     the "探测量" KPI instead.
var nonProviderChannels = map[string]bool{
	"http":  true,
	"probe": true,
}

// IsProviderChannel reports whether a channel is one an operator can add an
// account to and route a model through.
func IsProviderChannel(channel string) bool {
	name := strings.ToLower(strings.TrimSpace(channel))
	if name == "" {
		return false
	}
	return !nonProviderChannels[name]
}

// HandleOpsOverview answers the operations overview: KPI totals, a per-minute
// trend and the current alert set. Every number states its sample count, so the
// UI can show "暂无样本" instead of a healthy-looking zero.
func (a *API) HandleOpsOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	window, since, until := a.parseOpsWindow(r)
	channels, aggregates, _ := a.opsChannels(r.Context(), r, since, until)
	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("channel")))

	payload := map[string]interface{}{
		"window_minutes": window,
		"since":          since.UTC().Format(time.RFC3339),
		"until":          until.UTC().Format(time.RFC3339),
		"channels":       channels,
		// Named so the page can explain why some traffic is counted but not shown
		// as a channel (the http catch-all and our own synthetic probes).
		"excluded_aggregates": aggregates,
		"channel":        target,
		"totals":         opsagg.Summary{},
		"series":         []map[string]interface{}{},
		"alerts":         []alerting.Alert{},
		"coverage":       a.auditCoverage(r.Context()),
		"aggregation":    "per-minute",
		"retention_hours": int(opsagg.BucketRetention.Hours()),
	}

	if a.opsAggregator == nil || !a.opsAggregator.Enabled() {
		payload["available"] = false
		payload["note"] = "指标聚合需要 Redis；当前部署未启用。"
		_ = json.NewEncoder(w).Encode(payload)
		return
	}
	payload["available"] = true

	scope := target
	if scope == "" {
		scope = "all"
	}
	buckets, err := a.opsBuckets(r.Context(), scope, since, until)
	if err != nil {
		http.Error(w, "failed to read metric buckets", http.StatusInternalServerError)
		return
	}
	payload["totals"] = a.opsAggregator.Summarize(r.Context(), scope, buckets)
	payload["series"] = opsSeries(buckets)
	payload["matrix"] = a.opsMatrix(r.Context(), channels, since, until)
	payload["alerts"] = a.firingAlerts()
	payload["concurrency"] = a.currentConcurrency()

	_ = json.NewEncoder(w).Encode(payload)
}

// HandleOpsChannels answers the channel × model status matrix on its own, which
// lets the page refresh it without recomputing the trend.
func (a *API) HandleOpsChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, since, until := a.parseOpsWindow(r)
	channels, aggregates, _ := a.opsChannels(r.Context(), r, since, until)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"matrix":  a.opsMatrix(r.Context(), channels, since, until),
		"excluded_aggregates": aggregates,
		"alerts":  a.firingAlerts(),
		"channels": channels,
	})
}

// HandleOpsAlerts answers the currently firing alerts.
func (a *API) HandleOpsAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	alerts := a.firingAlerts()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"alerts": alerts, "count": len(alerts)})
}

// HandleJournalRecords answers one journal tab. It is the modern counterpart of
// /api/audit: same ledger, but filtered by kind and joined with the upstream
// attempts its request produced.
func (a *API) HandleJournalRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.writeJournalList(w, r)
	case http.MethodPost:
		// Reserved for future journal actions (acknowledge an alert, annotate an
		// operation). Answering explicitly beats a confusing 405.
		http.Error(w, "no journal action is defined", http.StatusNotImplemented)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) writeJournalList(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		http.Error(w, "journal requires Redis storage", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind == "" {
		kind = string(audit.KindRequest)
	}
	maxID := "+"
	if before := strings.TrimSpace(r.URL.Query().Get("before")); before != "" {
		maxID = "(" + before
	}

	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"
	entries, err := client.XRevRangeN(r.Context(), key, maxID, "-", int64(limit)*6).Result()
	if err != nil {
		http.Error(w, "failed to read journal", http.StatusInternalServerError)
		return
	}
	filter := auditFilterFromQuery(r)
	filter.kind = kind

	records := make([]map[string]interface{}, 0, limit)
	attempts := map[string][]audit.Event{}
	scanned := 0
	for _, entry := range entries {
		event, ok := decodeAuditEvent(entry)
		if !ok {
			continue
		}
		// Collect the upstream attempts of this request even when the request
		// record itself is outside the page: the detail panel is built from them.
		if event.Kind == audit.KindRequest && event.Action == "grok_upstream_attempt" && event.RequestID != "" {
			attempts[event.RequestID] = append(attempts[event.RequestID], event)
			continue
		}
		if !filter.matches(event) {
			continue
		}
		scanned++
		records = append(records, map[string]interface{}{
			"id":       entry.ID,
			"event":    event,
			"attempts": attempts[event.RequestID],
		})
		if len(records) >= limit {
			break
		}
	}

	nextCursor := ""
	if len(records) == limit {
		nextCursor = records[len(records)-1]["id"].(string)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":        records,
		"next_cursor": nextCursor,
		"kind":        kind,
		"scanned":     scanned,
		"coverage":    a.auditCoverage(r.Context()),
	})
}

func decodeAuditEvent(entry redis.XMessage) (audit.Event, bool) {
	raw, _ := entry.Values["data"].(string)
	if raw == "" {
		return audit.Event{}, false
	}
	var event audit.Event
	if json.Unmarshal([]byte(raw), &event) != nil {
		return audit.Event{}, false
	}
	if event.Kind == "" {
		// Records written before the journal was split have no kind; inference
		// traffic used the request actions.
		switch {
		case strings.HasSuffix(event.Action, "_request"), event.Action == "grok_upstream_attempt", event.Action == "http_request":
			event.Kind = audit.KindRequest
		default:
			event.Kind = audit.KindOperation
		}
	}
	return event, true
}

// opsBuckets reads the per-channel buckets of one scope. "all" merges every
// channel that reported traffic in the window.
func (a *API) opsBuckets(ctx context.Context, scope string, since, until time.Time) ([]opsagg.Bucket, error) {
	if scope == "all" {
		channels, err := a.opsAggregator.Channels(ctx, since, until)
		if err != nil {
			return nil, err
		}
		merged := map[time.Time]opsagg.Bucket{}
		for _, channel := range channels {
			buckets, err := a.opsAggregator.Range(ctx, channel, since, until)
			if err != nil {
				return nil, err
			}
			for _, bucket := range buckets {
				combined := merged[bucket.Minute]
				combined.Minute = bucket.Minute
				combined.Requests += bucket.Requests
				combined.Success += bucket.Success
				combined.Failed += bucket.Failed
				combined.Probes += bucket.Probes
				combined.Input += bucket.Input
				combined.Output += bucket.Output
				merged[bucket.Minute] = combined
			}
		}
		out := make([]opsagg.Bucket, 0, len(merged))
		for _, bucket := range merged {
			out = append(out, bucket)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Minute.Before(out[j].Minute) })
		return out, nil
	}
	return a.opsAggregator.Range(ctx, scope, since, until)
}

func opsSeries(buckets []opsagg.Bucket) []map[string]interface{} {
	series := make([]map[string]interface{}, 0, len(buckets))
	for _, bucket := range buckets {
		series = append(series, map[string]interface{}{
			"minute":   bucket.Minute.UTC().Format(time.RFC3339),
			"requests": bucket.Requests,
			"success":  bucket.Success,
			"failed":   bucket.Failed,
			"probes":   bucket.Probes,
		})
	}
	return series
}

// opsMatrix builds the channel × model status rows. A row with no samples is
// reported as such (samples = 0) instead of a green, traffic-free channel.
func (a *API) opsMatrix(ctx context.Context, channels []string, since, until time.Time) []map[string]interface{} {
	accounts, _ := a.store.ListAccounts(ctx)
	now := time.Now()

	rows := make([]map[string]interface{}, 0, len(channels))
	for _, channel := range channels {
		// The matrix answers "which provider channel × model is healthy", so the
		// infrastructure aggregates are not rows in it.
		if !IsProviderChannel(channel) {
			continue
		}
		enabled, available, needingLogin, modelCooldowns := poolCounts(accounts, channel, now)
		row := map[string]interface{}{
			"channel":               channel,
			"accounts_enabled":      enabled,
			"accounts_available":    available,
			"accounts_needing_login": needingLogin,
			"model_cooldowns":       modelCooldowns,
			"models":                []opsagg.ModelStats{},
		}
		if a.opsAggregator != nil && a.opsAggregator.Enabled() {
			buckets, err := a.opsAggregator.Range(ctx, channel, since, until)
			if err == nil {
				summary := a.opsAggregator.Summarize(ctx, channel, buckets)
				row["summary"] = summary
				row["models"] = a.opsAggregator.ModelStatsFromBuckets(ctx, channel, buckets)
				row["has_sample"] = summary.Requests > 0
				row["series"] = opsSeries(buckets)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// poolCounts summarizes a channel's account pool for the matrix.
func poolCounts(accounts []*store.Account, channel string, now time.Time) (enabled, available, needingLogin, modelCooldowns int) {
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), channel) {
			continue
		}
		// Linked Console companions are internal plumbing; the visible source owns
		// the channel's health.
		if acc.GrokSSOParentID != 0 {
			continue
		}
		if !acc.Enabled {
			continue
		}
		enabled++
		if accountpolicy.AccountHeld(acc, now) {
			continue
		}
		available++
		if accountpolicy.NeedsReverify(acc, now) {
			needingLogin++
		}
		for model, until := range acc.ModelCooldowns {
			if strings.TrimSpace(model) != "" && until.After(now) {
				modelCooldowns++
			}
		}
	}
	return enabled, available, needingLogin, modelCooldowns
}

func (a *API) firingAlerts() []alerting.Alert {
	if a == nil || a.alertEngine == nil {
		return []alerting.Alert{}
	}
	alerts := a.alertEngine.Firing()
	if alerts == nil {
		return []alerting.Alert{}
	}
	return alerts
}

// currentConcurrency reports how many accounts are mid-refresh right now, if the
// deployment wired a reporter.
func (a *API) currentConcurrency() map[string]interface{} {
	value := 0
	if a != nil && a.refreshConcurrency != nil {
		value = a.refreshConcurrency()
	}
	return map[string]interface{}{"accounts_refreshing": value}
}

// parseOpsWindow resolves the requested window, clamped to what the buckets
// retain, and returns the resolved boundaries so the page can state them.
func (a *API) parseOpsWindow(r *http.Request) (int, time.Time, time.Time) {
	minutes := defaultOpsWindowMinutes
	if raw := strings.TrimSpace(r.URL.Query().Get("window")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			minutes = parsed
		}
	}
	if minutes > opsagg.MaxTrendMinutes {
		minutes = opsagg.MaxTrendMinutes
	}
	if maxMinutes := int(opsagg.BucketRetention.Minutes()); minutes > maxMinutes {
		minutes = maxMinutes
	}
	until := time.Now()
	if raw := strings.TrimSpace(r.URL.Query().Get("until")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			until = parsed
		}
	}
	return minutes, until.Add(-time.Duration(minutes) * time.Minute), until
}

// opsChannels lists the provider channels the page may filter by, plus the
// infrastructure aggregates that were counted but left out, so the page can say
// what it excluded instead of silently dropping traffic from the picker.
func (a *API) opsChannels(ctx context.Context, r *http.Request, since, until time.Time) ([]string, []string, error) {
	seen := map[string]bool{}
	if a.opsAggregator != nil && a.opsAggregator.Enabled() {
		observed, err := a.opsAggregator.Channels(ctx, since, until)
		if err != nil {
			return nil, nil, err
		}
		for _, channel := range observed {
			seen[channel] = true
		}
	}
	if a.store != nil {
		if accounts, err := a.store.ListAccounts(ctx); err == nil {
			for _, acc := range accounts {
				if acc == nil || acc.GrokSSOParentID != 0 {
					continue
				}
				name := strings.ToLower(strings.TrimSpace(acc.AccountType))
				if name != "" {
					seen[name] = true
				}
			}
		}
	}
	channels := make([]string, 0, len(seen))
	aggregates := make([]string, 0, 2)
	for channel := range seen {
		if IsProviderChannel(channel) {
			channels = append(channels, channel)
			continue
		}
		aggregates = append(aggregates, channel)
	}
	sort.Strings(channels)
	sort.Strings(aggregates)
	return channels, aggregates, nil
}
