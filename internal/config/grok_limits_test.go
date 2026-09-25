package config

import (
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestGrokBuildLimitsSurviveConfigRoundTrip(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"max_retries":2,"retry_delay":50,"account_switch_count":4,"request_timeout":1800,"concurrency_timeout":2400,"retry_429_interval":90,"grok_build_rps":10,"grok_build_timeout_seconds":1800,"grok_stream_idle_seconds":300,"warp_stream_idle_seconds":420}`), &cfg); err != nil {
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
	if restored.GrokRequestsPerSecond("build") != 10 || restored.GrokRequestTimeout("build") != 1800*time.Second {
		t.Fatal("Build limits lost")
	}
	if restored.GrokStreamIdleTimeoutFor("build") != 300*time.Second {
		t.Fatal("idle setting lost")
	}
	if restored.WarpStreamIdleTimeout() != 420*time.Second {
		t.Fatal("Warp idle settings lost")
	}
}

func TestGrokBuildLimitsDefaultsAndBounds(t *testing.T) {
	var cfg *Config
	if cfg.GrokRequestsPerSecond("build") != 0 || cfg.GrokRequestTimeout("build") != 600*time.Second {
		t.Fatal("unexpected defaults")
	}
	cfg = &Config{RequestTimeout: 999999, GrokBuildTimeout: 999999, GrokBuildRPS: 999999, GrokStreamIdleSeconds: 999999, WarpStreamIdleSeconds: 999999}
	ApplyHardcoded(cfg)
	if cfg.RequestTimeout != 86400 || cfg.GrokRequestTimeout("build") != 24*time.Hour || cfg.GrokStreamIdleTimeoutFor("build") != 10*time.Minute || cfg.GrokRequestsPerSecond("build") != 1000 {
		t.Fatal("invalid bounds")
	}
	if cfg.WarpStreamIdleTimeout() != time.Hour {
		t.Fatal("invalid Warp idle bounds")
	}
}

func TestGrokBuildIdleDefaultsOverridesAndLegacyFallback(t *testing.T) {
	var cfg *Config
	if cfg.GrokStreamIdleTimeoutFor("build") != 2*time.Minute {
		t.Fatal("unexpected Build default")
	}
	cfg = &Config{GrokStreamIdleSeconds: 45, GrokBuildStreamIdleSeconds: 9999}
	if cfg.GrokStreamIdleTimeoutFor("build") != 10*time.Minute {
		t.Fatal("Build override broken")
	}
	cfg.GrokBuildStreamIdleSeconds = 1
	if cfg.GrokStreamIdleTimeoutFor("build") != 30*time.Second {
		t.Fatal("minimum idle bound broken")
	}
}
