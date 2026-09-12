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
func probeTargets(accounts []*store.Account) []probeTarget {
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

	targets := make([]probeTarget, 0, len(channels))
	for channel := range channels {
		switch channel {
		case "grok":
			targets = append(targets, probeTarget{
				Channel: "grok",
				// The reserved model name marks the traffic as synthetic, so the
				// overview counts it apart from real requests.
				Model:   probeModelLabel + channel,
				Path:    "/v1/responses",
				Payload: `{"model":"grok-4.6","input":"ping","stream":false,"max_output_tokens":16}`,
			})
		default:
			// Other channels are covered by their refresh loops; probing them here
			// would need a per-channel request shape.
			continue
		}
	}
	return targets
}

// startProbeLoop issues periodic probes and records their outcome in the journal
// as a system event. Probe results never decide account health: a synthetic
// failure must not disable a credential that real traffic is using successfully.
func startProbeLoop(ctx context.Context, s *store.Store, logger audit.Logger, port string) {
	if s == nil || strings.TrimSpace(port) == "" {
		return
	}
	base := "http://127.0.0.1:" + strings.TrimSpace(port)
	client := &http.Client{Timeout: 45 * time.Second}

	run := func() {
		accounts, err := s.ListAccounts(ctx)
		if err != nil {
			return
		}
		for _, target := range probeTargets(accounts) {
			started := time.Now()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+target.Path, strings.NewReader(target.Payload))
			if err != nil {
				continue
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(middleware.ProbeHeader, "1")
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
			if logger == nil {
				continue
			}
			logger.Log(ctx, audit.Event{
				Kind:    audit.KindSystem,
				Action:  "channel_probe",
				Channel: target.Channel,
				Model:   target.Model,
				Status:  status,
				Error:   detail,
				Duration: time.Since(started).Milliseconds(),
			})
		}
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in probe loop", "error", r)
			}
		}()
		ticker := time.NewTicker(probeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
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
