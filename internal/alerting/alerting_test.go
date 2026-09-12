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
			c.Success = 8
			c.Failed = 2
			c.SuccessRate = 0.8 // warning band
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
