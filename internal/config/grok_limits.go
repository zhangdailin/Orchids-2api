package config

import (
	"math"
	"time"
)

func boundedDefault(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(value, maximum)
}

// Pacing is optional and scoped to an account/team, never the whole endpoint.
func (c *Config) GrokRequestsPerSecond(provider string) float64 {
	if c == nil {
		return 0
	}
	var value float64
	switch provider {
	case "web":
		value = c.GrokWebRPS
	case "console":
		value = c.GrokConsoleRPS
	case "build":
		value = c.GrokBuildRPS
	}
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return min(max(value, 0.01), 1000)
}

// HTTP total deadline (including response body). The ingress request deadline
// remains independently controlled by concurrency_timeout.
func (c *Config) GrokRequestTimeout(provider string) time.Duration {
	value, fallback := 0, 600
	if c != nil {
		fallback = boundedDefault(c.RequestTimeout, 600, 86400)
		switch provider {
		case "web", "app_chat":
			value = c.GrokWebTimeout
		case "console":
			value = c.GrokConsoleTimeout
		case "build", "cli":
			value = c.GrokBuildTimeout
		}
	}
	return time.Duration(boundedDefault(value, fallback, 86400)) * time.Second
}

// GrokStreamIdleTimeout keeps the legacy API and represents Build semantics.
func (c *Config) GrokStreamIdleTimeout() time.Duration {
	return c.GrokStreamIdleTimeoutFor("build")
}

// GrokStreamIdleTimeoutFor returns a channel-specific inactivity budget. A
// legacy grok_stream_idle_seconds value remains an all-channel fallback.
func (c *Config) GrokStreamIdleTimeoutFor(provider string) time.Duration {
	value, fallback := 0, 120
	if provider == "web" || provider == "app_chat" {
		fallback = 90
	}
	if c != nil {
		value = c.GrokStreamIdleSeconds
		switch provider {
		case "web", "app_chat":
			if c.GrokWebStreamIdleSeconds > 0 {
				value = c.GrokWebStreamIdleSeconds
			}
		case "console":
			if c.GrokConsoleStreamIdleSeconds > 0 {
				value = c.GrokConsoleStreamIdleSeconds
			}
		case "build", "cli":
			if c.GrokBuildStreamIdleSeconds > 0 {
				value = c.GrokBuildStreamIdleSeconds
			}
		}
	}
	seconds := boundedDefault(value, fallback, 600)
	if seconds < 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}
