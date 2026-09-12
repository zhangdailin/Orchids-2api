package alerting

import (
	"sync"
	"testing"
	"time"
)

func channel(name string, mutate func(*ChannelSnapshot)) ChannelSnapshot {
	snapshot := ChannelSnapshot{Channel: name, Requests: 10, Success: 10, Samples: 10, SuccessRate: 1, AccountsEnabled: 3, AccountsAvailable: 3}
	if mutate != nil {
		mutate(&snapshot)
	}
	return snapshot
}

func engineWithRecorder() (*Engine, *[]string) {
	var mu sync.Mutex
	recorded := []string{}
	engine := NewEngine(DefaultRules(), func(alert Alert, firing bool) {
		mu.Lock()
		defer mu.Unlock()
		state := "fired"
		if !firing {
			state = "recovered"
		}
		recorded = append(recorded, state+":"+alert.Key)
	})
	return engine, &recorded
}

// TestEvaluate_FiresRecoversAndClosesTheLoop is the acceptance rule for stage 3:
// a condition raises an alert once, stays quiet while it persists, and produces a
// matching recovery when it clears.
func TestEvaluate_FiresRecoversAndClosesTheLoop(t *testing.T) {
	engine, recorded := engineWithRecorder()
	at := time.Now()

	// Healthy: nothing fires.
	if transition := engine.Evaluate(Snapshot{At: at, Channels: []ChannelSnapshot{channel("grok", nil)}}); len(transition.Firing) != 0 {
		t.Fatalf("healthy snapshot fired %+v", transition.Firing)
	}

	// Degraded: one critical alert.
	broken := Snapshot{At: at, Channels: []ChannelSnapshot{channel("grok", func(c *ChannelSnapshot) {
		c.Success = 2
		c.Failed = 8
		c.SuccessRate = 0.2
	})}}
	transition := engine.Evaluate(broken)
	if len(transition.Firing) != 1 || transition.Firing[0].Severity != SeverityCritical {
		t.Fatalf("degraded transition = %+v", transition)
	}
	// Still degraded: no re-announcement.
	if again := engine.Evaluate(broken); len(again.Firing) != 0 {
		t.Fatalf("a persisting condition must not re-fire: %+v", again.Firing)
	}
	// Recovered: exactly one recovery for the alert that fired.
	recovered := engine.Evaluate(Snapshot{At: at, Channels: []ChannelSnapshot{channel("grok", nil)}})
	if len(recovered.Recovered) != 1 || recovered.Recovered[0].Key != transition.Firing[0].Key {
		t.Fatalf("recovery = %+v", recovered.Recovered)
	}
	if len(engine.Firing()) != 0 {
		t.Fatalf("engine still reports firing alerts: %+v", engine.Firing())
	}

	want := []string{"fired:success-rate:grok", "recovered:success-rate:grok"}
	got := *recorded
	if len(got) != len(want) {
		t.Fatalf("recorded = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded = %v, want %v", got, want)
		}
	}
}

// TestEvaluate_QuietChannelNeverFires keeps a single failure on a quiet channel
// from paging anyone.
func TestEvaluate_QuietChannelNeverFires(t *testing.T) {
	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("puter", func(c *ChannelSnapshot) {
			c.Requests = 2
			c.Success = 0
			c.Failed = 2
			c.SuccessRate = 0
			c.Samples = 2
		}),
	}}, nil, DefaultRules())
	if len(transition.Firing) != 0 {
		t.Fatalf("a channel below the sample floor fired: %+v", transition.Firing)
	}
}

// TestEvaluate_PoolAndCredentialAlerts covers the two account-level rules.
func TestEvaluate_PoolAndCredentialAlerts(t *testing.T) {
	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("grok", func(c *ChannelSnapshot) {
			c.AccountsAvailable = 0
			c.AccountsNeedingLogin = 2
		}),
	}}, nil, DefaultRules())

	byKey := map[string]Alert{}
	for _, alert := range transition.Firing {
		byKey[alert.Key] = alert
	}
	if byKey["pool-empty:grok"].Severity != SeverityCritical {
		t.Fatalf("missing pool alert: %+v", transition.Firing)
	}
	if byKey["credential:grok"].Severity != SeverityCritical {
		t.Fatalf("missing credential alert: %+v", transition.Firing)
	}
}

// TestEvaluate_NoSampleIsNotHealthy confirms an idle channel raises nothing —
// there is nothing to alert about and nothing to claim as healthy either.
func TestEvaluate_NoSampleIsNotHealthy(t *testing.T) {
	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("warp", func(c *ChannelSnapshot) {
			c.Requests = 0
			c.Success = 0
			c.Failed = 0
			c.Samples = 0
			c.SuccessRate = 0
			c.AccountsEnabled = 0
			c.AccountsAvailable = 0
		}),
	}}, nil, DefaultRules())
	if len(transition.Firing) != 0 {
		t.Fatalf("an idle channel fired: %+v", transition.Firing)
	}
}

// TestEvaluate_SeverityOrdering puts the most urgent alert first.
func TestEvaluate_SeverityOrdering(t *testing.T) {
	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("grok", func(c *ChannelSnapshot) {
			c.Success = 7
			c.Failed = 3
			c.SuccessRate = 0.7 // warning band, with enough failures to count
		}),
		channel("warp", func(c *ChannelSnapshot) {
			c.AccountsNeedingLogin = 1 // critical
		}),
	}}, nil, DefaultRules())

	if len(transition.Firing) != 2 {
		t.Fatalf("firing = %+v", transition.Firing)
	}
	if transition.Firing[0].Severity != SeverityCritical {
		t.Fatalf("critical alert must sort first: %+v", transition.Firing)
	}
}

// TestEvaluate_IgnoresInfrastructureAggregates keeps the public-facing request
// aggregate out of alerting: /admin redirects and scanner 404s are not upstream
// health, and a visitor must not be able to page an operator.
func TestEvaluate_IgnoresInfrastructureAggregates(t *testing.T) {
	if IsAlertableChannel("http") || IsAlertableChannel("probe") {
		t.Fatal("the http and probe aggregates must not be alertable")
	}
	for _, channel := range []string{"grok", "warp", "puter", "workbuddy", "GROK"} {
		if !IsAlertableChannel(channel) {
			t.Fatalf("%s must remain alertable", channel)
		}
	}

	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("http", func(c *ChannelSnapshot) {
			// All failing, and enough traffic: without the exclusion this fires.
			c.Requests = 200
			c.Success = 20
			c.Failed = 180
			c.SuccessRate = 0.1
		}),
		channel("probe", func(c *ChannelSnapshot) {
			c.Requests = 50
			c.Success = 0
			c.Failed = 50
			c.SuccessRate = 0
		}),
		channel("grok", nil),
	}}, nil, DefaultRules())

	if len(transition.Firing) != 0 {
		t.Fatalf("infrastructure aggregates fired alerts: %+v", transition.Firing)
	}
}

// TestEvaluate_StillAlertsOnRealChannels is the counterweight: excluding the
// aggregates must not silence a provider channel.
func TestEvaluate_StillAlertsOnRealChannels(t *testing.T) {
	transition := Evaluate(Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("warp", func(c *ChannelSnapshot) {
			c.Requests = 40
			c.Success = 4
			c.Failed = 36
			c.SuccessRate = 0.1
		}),
	}}, nil, DefaultRules())
	if len(transition.Firing) != 1 || transition.Firing[0].Key != "success-rate:warp" {
		t.Fatalf("a real channel must still alert: %+v", transition.Firing)
	}
}

// TestEvaluate_HysteresisStopsFlapping is the fix for what production showed:// workbuddy hovered at 89% around the 90% line, so the alert fired and cleared on
// every evaluation and became noise. The alert must hold until the rate clears
// the threshold plus the margin.
func TestEvaluate_HysteresisStopsFlapping(t *testing.T) {
	rules := DefaultRules()
	at := time.Now()
	degraded := func(rate float64) Snapshot {
		return Snapshot{At: at, Channels: []ChannelSnapshot{
			channel("workbuddy", func(c *ChannelSnapshot) {
				c.Requests = 20
				c.SuccessRate = rate
				c.Samples = 20
				c.Success = int64(rate * 20)
				c.Failed = 20 - c.Success
			}),
		}}
	}

	// 89% is below the 90% warning line: fires.
	first := Evaluate(degraded(0.89), nil, rules)
	if len(first.Firing) != 1 {
		t.Fatalf("expected a warning at 89%%: %+v", first)
	}
	firing := map[string]Alert{}
	for _, alert := range first.Firing {
		firing[alert.Key] = alert
	}

	// 91% is above the line but inside the margin: the alert must stay, and must
	// not be re-announced (a re-announcement means "fired" again).
	still := Evaluate(degraded(0.91), firing, rules)
	if len(still.Firing) != 0 {
		t.Fatalf("a held alert must not re-announce: %+v", still.Firing)
	}
	if len(still.Recovered) != 0 {
		t.Fatalf("91%% is inside the hysteresis band and must not clear: %+v", still.Recovered)
	}

	// 95% is above the line plus the margin: it clears.
	cleared := Evaluate(degraded(0.95), firing, rules)
	if len(cleared.Recovered) != 1 {
		t.Fatalf("95%% must clear the alert: %+v", cleared.Recovered)
	}

	// Without an existing alert, 91% raises nothing (no flapping on the way up).
	if len(Evaluate(degraded(0.91), nil, rules).Firing) != 0 {
		t.Fatal("91% must not raise a new alert")
	}
}

// TestEvaluate_WarningBandNeedsEnoughFailures pins the noise production showed:
// "窗口内 9 次请求，失败 1 次（阈值 <90%）" fired and cleared every few minutes.
// One failure is not evidence that a channel is degraded — nine requests carry no
// statistical weight — so the warning band waits for MinFailures.
func TestEvaluate_WarningBandNeedsEnoughFailures(t *testing.T) {
	rules := DefaultRules()

	// The exact live case: 8 of 9 good, 1 bad. Above the sample floor, below the
	// warning line, and still not alertable.
	quiet := Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("grok", func(c *ChannelSnapshot) {
			c.Requests = 9
			c.Success = 8
			c.Failed = 1
			c.SuccessRate = 8.0 / 9.0
			c.Samples = 9
		}),
	}}
	if firing := Evaluate(quiet, nil, rules).Firing; len(firing) != 0 {
		t.Fatalf("a single failure in a quiet window fired: %+v", firing)
	}

	// The same ratio with enough failures behind it does fire.
	noisy := Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("grok", func(c *ChannelSnapshot) {
			c.Requests = 30
			c.Success = 26
			c.Failed = 4
			c.SuccessRate = 26.0 / 30.0
			c.Samples = 30
		}),
	}}
	if firing := Evaluate(noisy, nil, rules).Firing; len(firing) != 1 {
		t.Fatalf("4 failures in 30 requests (87%%) must fire: %+v", firing)
	}
}

// TestEvaluate_SevereOutageIgnoresTheFailureFloor is the counterweight to the rule
// above: a channel that answers correctly for almost nobody is broken whatever the
// count, so the critical band must not be gated by MinFailures. (With the shipped
// defaults the two thresholds overlap; the valve matters as soon as an operator
// raises MinFailures without raising the critical line with it.)
func TestEvaluate_SevereOutageIgnoresTheFailureFloor(t *testing.T) {
	rules := DefaultRules()
	rules.MinFailures = 5 // an operator who wants five failures before a warning

	severe := Snapshot{At: time.Now(), Channels: []ChannelSnapshot{
		channel("warp", func(c *ChannelSnapshot) {
			c.Requests = 5
			c.Success = 2
			c.Failed = 3 // below the configured failure floor, but only 40% success
			c.SuccessRate = 0.4
			c.Samples = 5
		}),
	}}
	firing := Evaluate(severe, nil, rules).Firing
	if len(firing) != 1 || firing[0].Severity != SeverityCritical {
		t.Fatalf("a 40%% success rate must fire as critical: %+v", firing)
	}
}

// TestEvaluate_FailureFloorDoesNotReleaseAFiringAlert keeps the interaction with
// hysteresis honest: once an alert is firing, a window that dips back under the
// failure floor is still inside the hold band, so it neither re-announces nor
// clears early.
func TestEvaluate_FailureFloorDoesNotReleaseAFiringAlert(t *testing.T) {
	rules := DefaultRules()
	at := time.Now()
	previous := map[string]Alert{"success-rate:grok": {Key: "success-rate:grok", Channel: "grok"}}
	held := Snapshot{At: at, Channels: []ChannelSnapshot{
		channel("grok", func(c *ChannelSnapshot) {
			c.Requests = 20
			c.Success = 18
			c.Failed = 2
			c.SuccessRate = 0.9 // exactly on the line: inside the hold band
			c.Samples = 20
		}),
	}}
	transition := Evaluate(held, previous, rules)
	if len(transition.Firing) != 0 {
		t.Fatalf("a held alert re-announced: %+v", transition.Firing)
	}
	if len(transition.Recovered) != 0 {
		t.Fatalf("a held alert cleared although the rate had not recovered: %+v", transition.Recovered)
	}
}
