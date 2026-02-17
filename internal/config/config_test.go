package config

import (
	"encoding/json"
	"testing"
)

func TestConfigDefaultsForGrok2APIParity(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)

	if got := cfg.ChatDefaultStream(); got != true {
		t.Fatalf("ChatDefaultStream()=%v want=true", got)
	}
	if got := cfg.PublicImagineNSFW(); got != true {
		t.Fatalf("PublicImagineNSFW()=%v want=true", got)
	}
	if got := cfg.PublicImagineFinalMinBytes(); got != 100000 {
		t.Fatalf("PublicImagineFinalMinBytes()=%d want=100000", got)
	}
	if got := cfg.PublicImagineMediumMinBytes(); got != 30000 {
		t.Fatalf("PublicImagineMediumMinBytes()=%d want=30000", got)
	}
	if got := cfg.PublicAPIEnabled(); got != false {
		t.Fatalf("PublicAPIEnabled()=%v want=false", got)
	}
	if got := cfg.PublicAPIKey(); got != "" {
		t.Fatalf("PublicAPIKey()=%q want empty", got)
	}
}

func TestConfigDotKeyAliasesAndPrecedence(t *testing.T) {
	raw := []byte(`{
		"stream": false,
		"app_stream": true,
		"app.stream": false,
		"image_nsfw": false,
		"image.nsfw": true,
		"image_final_min_bytes": 11111,
		"image.final_min_bytes": 22222,
		"image_medium_min_bytes": 33333,
		"image.medium_min_bytes": 44444,
		"public_key": "raw-public",
		"app_public_key": "app-public",
		"app.public_key": "dot-public",
		"public_enabled": false,
		"app_public_enabled": true,
		"app.public_enabled": false
	}`)
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal config failed: %v", err)
	}
	ApplyDefaults(&cfg)

	// app.stream > app_stream > stream
	if got := cfg.ChatDefaultStream(); got != false {
		t.Fatalf("ChatDefaultStream()=%v want=false", got)
	}
	// image.nsfw > image_nsfw
	if got := cfg.PublicImagineNSFW(); got != true {
		t.Fatalf("PublicImagineNSFW()=%v want=true", got)
	}
	// image.final_min_bytes > image_final_min_bytes
	if got := cfg.PublicImagineFinalMinBytes(); got != 22222 {
		t.Fatalf("PublicImagineFinalMinBytes()=%d want=22222", got)
	}
	// image.medium_min_bytes > image_medium_min_bytes
	if got := cfg.PublicImagineMediumMinBytes(); got != 44444 {
		t.Fatalf("PublicImagineMediumMinBytes()=%d want=44444", got)
	}
	// app.public_key > app_public_key > public_key
	if got := cfg.PublicAPIKey(); got != "dot-public" {
		t.Fatalf("PublicAPIKey()=%q want=dot-public", got)
	}
	// app.public_enabled > app_public_enabled > public_enabled
	if got := cfg.PublicAPIEnabled(); got != false {
		t.Fatalf("PublicAPIEnabled()=%v want=false", got)
	}
}
