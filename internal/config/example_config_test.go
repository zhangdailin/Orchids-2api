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
