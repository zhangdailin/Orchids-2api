package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigLoads keeps config.example.json honest. The file is what an
// operator copies to config.json on a new host, so a type that no longer matches
// the Config struct turns "deploy" into a service that exits during startup —
// which is exactly what a bare `"token_cache_strategy": 1` did before this test.
func TestExampleConfigLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json does not unmarshal into config.Config: %v", err)
	}
	if cfg.Port == "" || cfg.StoreMode == "" {
		t.Fatalf("config.example.json is missing required values: port=%q store_mode=%q", cfg.Port, cfg.StoreMode)
	}
}

// TestExampleConfigUpstreamURLsMatchDefaults pins the example's upstream
// endpoints to the code defaults. These are the values that drift when a
// provider moves a route: xAI renamed the device-authorization endpoint, the
// Go default followed, and the example kept pointing at the retired
// /oauth2/device/auth path. An operator who copies the example then gets a
// Grok login that fails with an upstream 404, surfaced through Caddy and
// Cloudflare as an opaque 502. Each field is checked only when the example
// actually carries it, so a deliberate omission still passes.
func TestExampleConfigUpstreamURLsMatchDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json does not unmarshal into config.Config: %v", err)
	}
	var empty Config
	for _, tc := range []struct {
		field string
		got   string
		want  string
	}{
		{"grok_cli_oauth_device_url", cfg.GrokCLIOAuthDeviceURL, empty.GrokCLIOAuthDeviceURLOrDefault()},
		{"grok_cli_oauth_token_url", cfg.GrokCLIOAuthTokenURL, empty.GrokCLIOAuthTokenURLOrDefault()},
		{"grok_cli_base_url", cfg.GrokCLIBaseURL, empty.GrokCLIBaseURLOrDefault()},
	} {
		if tc.got == "" {
			continue
		}
		if tc.got != tc.want {
			t.Errorf("config.example.json %s = %q, but the code default is %q; "+
				"the example must not pin a retired upstream route", tc.field, tc.got, tc.want)
		}
	}
}
