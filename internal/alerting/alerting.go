// Package alerting turns an operations snapshot into alerts and recoveries.
//
// It is deliberately stateless: evaluation is a pure function of the snapshot
// and the previously seen state, so the same inputs always produce the same
// alerts and a restart cannot invent or lose a firing alert. Fired and recovered
// transitions are written to the audit journal as system events, which is what
// makes "故障和恢复形成闭环" traceable.
package alerting

import (
	"fmt"
	"strings"
	"time"
)

// Severity levels, ordered by urgency.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// ChannelSnapshot is the per-channel evidence a rule may inspect.
type ChannelSnapshot struct {
	Channel string `json:"channel"`
	// Requests counts real (non-probe) requests in the window.
	Requests int64   `json:"requests"`
	Success  int64   `json:"success"`
	Failed   int64   `json:"failed"`
	Samples  int64   `json:"samples"`
	SuccessRate float64 `json:"success_rate"`
	// AccountsEnabled / AccountsAvailable describe the pool for this channel.
	AccountsEnabled   int `json:"accounts_enabled"`
	AccountsAvailable int `json:"accounts_available"`
	// AccountsNeedingLogin counts credentials the upstream refused.
	AccountsNeedingLogin int `json:"accounts_needing_login"`
	// ModelFailures counts per-model throttles currently in effect.
	ModelCooldowns int `json:"model_cooldowns"`
}

// Snapshot is the whole input to one evaluation pass.
type Snapshot struct {
	At       time.Time
	Channels []ChannelSnapshot
}

// Alert is one firing condition.
type Alert struct {
	// Key is stable across evaluations so a firing alert is not re-announced and
	// its recovery can be matched to it.
	Key      string   `json:"key"`
	Severity Severity `json:"severity"`
	Channel  string   `json:"channel,omitempty"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail"`
}

// Transition is what an evaluation pass produced.
type Transition struct {
	Firing    []Alert `json:"firing"`
	Recovered []Alert `json:"recovered"`
}

// Rules are the thresholds. They live in one struct so an operator can see the
// whole alerting policy in one place instead of hunting through the engine.
type Rules struct {
	// MinRequests is how much real traffic a window needs before a success-rate
	// rule is allowed to fire. Without it a single failed request would raise an
	// alert on a quiet channel.
	MinRequests int64
	// SuccessRateWarning / SuccessRateCritical are ratios (0..1).
	SuccessRateWarning  float64
	SuccessRateCritical float64
	// RequireNoAvailableAccounts fires when a channel has enabled accounts but
	// none usable.
	RequireNoAvailableAccounts bool
}

// DefaultRules is the shipped policy.
func DefaultRules() Rules {
	return Rules{
		MinRequests:                5,
		SuccessRateWarning:         0.9,
		SuccessRateCritical:        0.5,
		RequireNoAvailableAccounts: true,
	}
}

// Evaluate compares a snapshot with the previously firing set and returns the
// nonAlertableChannels are aggregates that must never raise an alert.
//
// "http" collects every request whose path matched no provider prefix: the admin
// UI's own redirects, health checks, and whatever a public scanner tries. Its
// failure ratio therefore measures exposure to the internet rather than upstream
// health, so alerting on it meant a page visit could page the operator.
// "probe" is synthetic by definition and is judged by its own channel outcome,
// not by the request-outcome ratio.
var nonAlertableChannels = map[string]bool{
	"http":  true,
	"probe": true,
}

// IsAlertableChannel reports whether a channel takes part in alerting.
func IsAlertableChannel(channel string) bool {
	name := strings.ToLower(strings.TrimSpace(channel))
	if name == "" {
		return false
	}
	return !nonAlertableChannels[name]
}

// transitions. previous is keyed by Alert.Key.
func Evaluate(snapshot Snapshot, previous map[string]Alert, rules Rules) Transition {
	if rules.MinRequests <= 0 {
		rules.MinRequests = 1
	}
	now := snapshot.At
	if now.IsZero() {
		now = time.Now()
	}
	current := map[string]Alert{}

	for _, channel := range snapshot.Channels {
		name := strings.TrimSpace(channel.Channel)
		// Infrastructure aggregates are counted in the overview but never alerted
		// on: their failures belong to the internet, not to the upstream, and a
		// public scanner must not be able to raise a channel alert.
		if name == "" || !IsAlertableChannel(name) {
			continue
		}

		// Credential failures are critical: waiting cannot repair them.
		if channel.AccountsNeedingLogin > 0 {
			key := "credential:" + name
			current[key] = Alert{
				Key:      key,
				Severity: SeverityCritical,
				Channel:  name,
				Title:    fmt.Sprintf("%s 有 %d 个账号需要重新登录", name, channel.AccountsNeedingLogin),
				Detail:   "上游拒绝了这些账号的凭据，等待无法恢复，需要重新授权。",
			}
		}

		// Pool exhaustion.
		if rules.RequireNoAvailableAccounts && channel.AccountsEnabled > 0 && channel.AccountsAvailable == 0 {
			key := "pool-empty:" + name
			current[key] = Alert{
				Key:      key,
				Severity: SeverityCritical,
				Channel:  name,
				Title:    fmt.Sprintf("%s 没有可用账号", name),
				Detail:   fmt.Sprintf("%d 个启用账号全部处于冷却/异常状态。", channel.AccountsEnabled),
			}
		}

		// Success rate, only with enough real traffic to mean something.
		if channel.Requests >= rules.MinRequests && channel.Samples > 0 {
			rate := channel.SuccessRate
			switch {
			case rate < rules.SuccessRateCritical:
				key := "success-rate:" + name
				current[key] = Alert{
					Key:      key,
					Severity: SeverityCritical,
					Channel:  name,
					Title:    fmt.Sprintf("%s 成功率 %.0f%%", name, rate*100),
					Detail:   fmt.Sprintf("窗口内 %d 次请求，失败 %d 次（阈值 <%.0f%%）。", channel.Requests, channel.Failed, rules.SuccessRateCritical*100),
				}
			case rate < rules.SuccessRateWarning:
				key := "success-rate:" + name
				current[key] = Alert{
					Key:      key,
					Severity: SeverityWarning,
					Channel:  name,
					Title:    fmt.Sprintf("%s 成功率 %.0f%%", name, rate*100),
					Detail:   fmt.Sprintf("窗口内 %d 次请求，失败 %d 次（阈值 <%.0f%%）。", channel.Requests, channel.Failed, rules.SuccessRateWarning*100),
				}
			}
		}
	}

	transition := Transition{}
	for key, alert := range current {
		if _, wasFiring := previous[key]; wasFiring {
			continue
		}
		transition.Firing = append(transition.Firing, alert)
	}
	for key, alert := range previous {
		if _, stillFiring := current[key]; stillFiring {
			continue
		}
		recovered := alert
		recovered.Detail = "条件已恢复。"
		transition.Recovered = append(transition.Recovered, recovered)
	}
	sortAlerts(transition.Firing)
	sortAlerts(transition.Recovered)
	return transition
}

func sortAlerts(alerts []Alert) {
	rank := func(severity Severity) int {
		switch severity {
		case SeverityCritical:
			return 0
		case SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(alerts); i++ {
		for j := i; j > 0 && (rank(alerts[j].Severity) < rank(alerts[j-1].Severity) ||
			(rank(alerts[j].Severity) == rank(alerts[j-1].Severity) && alerts[j].Key < alerts[j-1].Key)); j-- {
			alerts[j], alerts[j-1] = alerts[j-1], alerts[j]
		}
	}
}

// Engine keeps the firing set between passes and records transitions.
type Engine struct {
	rules  Rules
	mu     chan struct{}
	firing map[string]Alert
	record func(alert Alert, firing bool)
}

// NewEngine creates an engine that reports transitions through record.
func NewEngine(rules Rules, record func(alert Alert, firing bool)) *Engine {
	engine := &Engine{
		rules:  rules,
		mu:     make(chan struct{}, 1),
		firing: map[string]Alert{},
		record: record,
	}
	engine.mu <- struct{}{}
	return engine
}

// Evaluate runs one pass and records firing/recovery transitions.
func (e *Engine) Evaluate(snapshot Snapshot) Transition {
	if e == nil {
		return Transition{}
	}
	<-e.mu
	previous := e.firing
	transition := Evaluate(snapshot, previous, e.rules)

	// The new firing set is "what was already firing and did not recover" plus
	// the alerts that just started.
	next := make(map[string]Alert, len(previous)+len(transition.Firing))
	for key, alert := range previous {
		next[key] = alert
	}
	for _, alert := range transition.Recovered {
		delete(next, alert.Key)
	}
	for _, alert := range transition.Firing {
		next[alert.Key] = alert
	}
	e.firing = next
	record := e.record
	e.mu <- struct{}{}

	if record != nil {
		for _, alert := range transition.Firing {
			record(alert, true)
		}
		for _, alert := range transition.Recovered {
			record(alert, false)
		}
	}
	return transition
}

// Firing returns a copy of the currently firing alerts.
func (e *Engine) Firing() []Alert {
	if e == nil {
		return nil
	}
	<-e.mu
	out := make([]Alert, 0, len(e.firing))
	for _, alert := range e.firing {
		out = append(out, alert)
	}
	e.mu <- struct{}{}
	sortAlerts(out)
	return out
}
