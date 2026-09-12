package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/alerting"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/store"
)

// alertWindowMinutes is the evidence window one evaluation pass looks at. It is
// long enough for a success-rate rule to have samples and short enough to notice
// a regression within a few minutes.
const alertWindowMinutes = 30

// alertEvery is how often rules are evaluated.
const alertEvery = 60 * time.Second

// startAlertLoop evaluates the alert rules and journals every firing and
// recovery, so a failure has a start, an owner and an end in one place.
func startAlertLoop(ctx context.Context, agg *opsagg.Aggregator, s *store.Store, engine *alerting.Engine, logger audit.Logger) {
	if agg == nil || !agg.Enabled() || engine == nil {
		slog.Debug("Alert evaluation disabled (no metric aggregation)")
		return
	}
	evaluate := func() {
		snapshot, err := buildAlertSnapshot(context.Background(), agg, s)
		if err != nil {
			slog.Warn("Alert evaluation failed", "error", err)
			return
		}
		transition := engine.Evaluate(snapshot)
		for _, alert := range transition.Firing {
			slog.Warn("Alert firing", "key", alert.Key, "severity", string(alert.Severity), "channel", alert.Channel, "title", alert.Title, "detail", alert.Detail)
		}
		for _, alert := range transition.Recovered {
			slog.Info("Alert recovered", "key", alert.Key, "channel", alert.Channel, "title", alert.Title)
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in alert loop", "error", r)
			}
		}()
		evaluate()
		ticker := time.NewTicker(alertEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				evaluate()
			}
		}
	}()
}

// buildAlertSnapshot assembles the evidence one evaluation pass needs: per
// channel request outcomes, pool availability and credential state.
func buildAlertSnapshot(ctx context.Context, agg *opsagg.Aggregator, s *store.Store) (alerting.Snapshot, error) {
	now := time.Now()
	since := now.Add(-time.Duration(alertWindowMinutes) * time.Minute)
	snapshot := alerting.Snapshot{At: now}

	channels, err := agg.Channels(ctx, since, now)
	if err != nil {
		return snapshot, err
	}
	accounts, accountErr := s.ListAccounts(ctx)
	if accountErr != nil {
		slog.Warn("Alert evaluation could not read accounts", "error", accountErr)
	}

	seen := map[string]bool{}
	add := func(channel string) alerting.ChannelSnapshot {
		buckets, rangeErr := agg.Range(ctx, channel, since, now)
		entry := alerting.ChannelSnapshot{Channel: channel}
		if rangeErr == nil {
			summary := agg.Summarize(ctx, channel, buckets)
			entry.Requests = summary.Requests - summary.Probes
			entry.Success = summary.Success
			entry.Failed = summary.Failed
			entry.Samples = summary.Samples
			entry.SuccessRate = summary.SuccessRate
		}
		entry.AccountsEnabled, entry.AccountsAvailable, entry.AccountsNeedingLogin, entry.ModelCooldowns = alertPoolCounts(accounts, channel, now)
		return entry
	}

	for _, channel := range channels {
		seen[channel] = true
		snapshot.Channels = append(snapshot.Channels, add(channel))
	}
	// A channel with accounts but no traffic still matters: an exhausted pool must
	// alert even when no request has been served yet.
	for _, channel := range alertChannels(accounts) {
		if seen[channel] {
			continue
		}
		snapshot.Channels = append(snapshot.Channels, add(channel))
	}
	return snapshot, nil
}

func alertChannels(accounts []*store.Account) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, acc := range accounts {
		if acc == nil || acc.GrokSSOParentID != 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(acc.AccountType))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func alertPoolCounts(accounts []*store.Account, channel string, now time.Time) (enabled, available, needingLogin, modelCooldowns int) {
	for _, acc := range accounts {
		if acc == nil || acc.GrokSSOParentID != 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), channel) || !acc.Enabled {
			continue
		}
		enabled++
		if !accountpolicy.AccountHeld(acc, now) {
			available++
		}
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

// probeModelLabel keeps a probe's journal entry recognisable as synthetic traffic.
const probeModelLabel = "__probe__"

// probeEvery is how often active probes run. Probes exist to answer "can this
// channel serve right now?" when there is no real traffic to observe.
const probeEvery = 5 * time.Minute

// probeTarget is one channel's probe definition.
type probeTarget struct {
	Channel string
	Model   string
	Path    string
	Payload string
}

// probeTargets returns the channels worth probing: those with at least one
// usable account. A channel with no accounts is reported by the pool rule, not
// by a probe that could only ever fail.
func probeTargets(accounts []*store.Account, cfg *config.Config) []probeTarget {
	now := time.Now()
	channels := map[string]string{}
	for _, acc := range accounts {
		if acc == nil || acc.GrokSSOParentID != 0 || !acc.Enabled {
			continue
		}
		channel := strings.ToLower(strings.TrimSpace(acc.AccountType))
		if channel == "" || accountpolicy.AccountHeld(acc, now) {
			continue
		}
		if _, exists := channels[channel]; !exists {
			channels[channel] = channel
		}
	}

	probeModel := strings.TrimSpace(cfg.GrokProbeModel)
	if probeModel == "" {
		// A cheap, widely available model: the probe measures reachability, not
		// capability, and must not consume a frontier quota.
		probeModel = defaultProbeModel
	}

	targets := make([]probeTarget, 0, len(channels))
	for channel := range channels {
		switch channel {
		case "grok":
			targets = append(targets, probeTarget{
				Channel: "grok",
				// The reserved label marks the traffic as synthetic, so the overview
				// counts it apart from real requests.
				Model:   probeModelLabel,
				Path:    "/v1/responses",
				Payload: `{"model":"` + probeModel + `","input":"ping","stream":false,"max_output_tokens":16}`,
			})
		default:
			// Other channels are covered by their refresh loops; probing them here
			// would need a per-channel request shape.
			continue
		}
	}
	return targets
}

// defaultProbeModel is the model a probe asks for unless one is configured.
const defaultProbeModel = "grok-4.6"

// resolveProbeAPIKey finds a credential the probe can authenticate with.
//
// With inference auth enabled every /v1 route requires a managed API key, so a
// probe without one was rejected by our own middleware and measured nothing about
// the upstream. The public key is the supported source: managed keys are stored
// hashed, so their plaintext cannot be recovered. When no key is configured,
// probing is skipped instead of generating a stream of local 401s.
func resolveProbeAPIKey(cfg *config.Config) string {
	if cfg == nil || !cfg.InferenceAuthEnabled() {
		return ""
	}
	return cfg.PublicAPIKey()
}

// probeStartDelay is how long after startup the first probe runs. The listener
// is bound after this loop starts, so an immediate probe would measure our own
// absent socket rather than the upstream.
const probeStartDelay = 90 * time.Second

// probeOnce runs one round of probes and returns how many were recorded. It is
// the testable core of the loop: the timing lives in startProbeLoop.
//
// Probe results never decide account health: a synthetic failure must not
// disable a credential that real traffic is using successfully.
func probeOnce(ctx context.Context, s *store.Store, cfg *config.Config, logger audit.Logger, base string, client *http.Client) int {
	if s == nil || cfg == nil || strings.TrimSpace(base) == "" {
		return 0
	}
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return 0
	}
	apiKey := resolveProbeAPIKey(cfg)
	if apiKey == "" && cfg.InferenceAuthEnabled() {
		// Without a key every probe would be rejected by our own auth middleware,
		// so the result would describe the middleware, not the upstream.
		slog.Debug("Channel probes skipped: inference auth is enabled but no public API key is configured")
		return 0
	}
	recorded := 0
	for _, target := range probeTargets(accounts, cfg) {
		started := time.Now()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+target.Path, strings.NewReader(target.Payload))
		if err != nil {
			continue
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(middleware.ProbeHeader, "1")
		if apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+apiKey)
		}
		response, err := client.Do(request)
		status := "error"
		detail := ""
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				status = "ok"
			} else {
				detail = response.Status
			}
		} else {
			detail = err.Error()
		}
		recorded++
		if logger == nil {
			continue
		}
		logger.Log(ctx, audit.Event{
			Kind:   audit.KindSystem,
			Action: "channel_probe",
			// A probe is not the probed channel's traffic: it is recorded as
			// synthetic so it can never be mistaken for a real request, and the
			// probed channel is named in Provider instead.
			Channel:  middleware.ProbeChannel,
			Provider: target.Channel,
			Model:    target.Model,
			Status:   status,
			Error:    detail,
			Duration: time.Since(started).Milliseconds(),
		})
	}
	return recorded
}

// startProbeLoop issues periodic probes and records their outcome in the journal
// as a system event.
func startProbeLoop(ctx context.Context, s *store.Store, cfg *config.Config, logger audit.Logger, port string) {
	if s == nil || cfg == nil || strings.TrimSpace(port) == "" {
		return
	}
	base := "http://127.0.0.1:" + strings.TrimSpace(port)
	client := &http.Client{Timeout: 45 * time.Second}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in probe loop", "error", r)
			}
		}()
		timer := time.NewTimer(probeStartDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			probeOnce(ctx, s, cfg, logger, base, client)
		}
		ticker := time.NewTicker(probeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				probeOnce(ctx, s, cfg, logger, base, client)
			}
		}
	}()
}
func newAuditAlertRecorder(logger audit.Logger) func(alerting.Alert, bool) {
	return func(alert alerting.Alert, firing bool) {
		if logger == nil {
			return
		}
		status := "firing"
		action := "alert_fired"
		errorText := alert.Detail
		if !firing {
			status = "recovered"
			action = "alert_recovered"
			errorText = ""
		}
		logger.Log(context.Background(), audit.Event{
			Kind:    audit.KindSystem,
			Action:  action,
			Channel: alert.Channel,
			Model:   alert.Key,
			Status:  status,
			Error:   errorText,
			Details: alert.Title,
			Metadata: map[string]interface{}{
				"severity": string(alert.Severity),
				"key":      alert.Key,
			},
		})
	}
}
