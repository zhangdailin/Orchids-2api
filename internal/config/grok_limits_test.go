package config

import (
	"github.com/goccy/go-json"
	"testing"
	"time"
)

func TestGrokLimitsSurviveConfigRoundTrip(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"max_retries":2,"retry_delay":50,"account_switch_count":4,"request_timeout":1800,"concurrency_timeout":2400,"retry_429_interval":90,"grok_web_rps":3,"grok_console_rps":0.5,"grok_build_rps":10,"grok_web_timeout_seconds":900,"grok_console_timeout_seconds":1200,"grok_build_timeout_seconds":1800,"grok_stream_idle_seconds":300}`), &cfg); err != nil {
		t.Fatal(err)
	}
	ApplyHardcoded(&cfg)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	ApplyHardcoded(&restored)
	if restored.MaxRetries != 2 || restored.RetryDelay != 50 || restored.AccountSwitchCount != 4 || restored.RequestTimeout != 1800 || restored.ConcurrencyTimeout != 2400 || restored.Retry429Interval != 90 {
		t.Fatal("runtime settings were overwritten")
	}
	for _, test := range []struct {
		provider string
		rps      float64
		seconds  int
	}{{"web", 3, 900}, {"console", 0.5, 1200}, {"build", 10, 1800}} {
		if restored.GrokRequestsPerSecond(test.provider) != test.rps || restored.GrokRequestTimeout(test.provider) != time.Duration(test.seconds)*time.Second {
			t.Fatal(test)
		}
	}
	if restored.GrokStreamIdleTimeout() != 300*time.Second {
		t.Fatal("idle setting lost")
	}
}

func TestGrokLimitsDefaultsAndBounds(t *testing.T) {
	var cfg *Config
	if cfg.GrokRequestsPerSecond("console") != 0 || cfg.GrokRequestTimeout("build") != 600*time.Second {
		t.Fatal("unexpected defaults")
	}
	cfg = &Config{RequestTimeout: 999999, GrokWebTimeout: 999999, GrokConsoleRPS: 999999, GrokBuildRPS: -1, GrokStreamIdleSeconds: 999999}
	ApplyHardcoded(cfg)
	if cfg.RequestTimeout != 86400 || cfg.GrokRequestTimeout("web") != 24*time.Hour || cfg.GrokStreamIdleTimeout() != time.Hour || cfg.GrokRequestsPerSecond("console") != 1000 || cfg.GrokRequestsPerSecond("build") != 0 {
		t.Fatal("invalid bounds")
	}
}
