package config

import (
	"path/filepath"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
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
	if got := cfg.PublicAPIEnabled(); got != true {
		t.Fatalf("PublicAPIEnabled()=%v want=true", got)
	}
	if got := cfg.PublicAPIKey(); got != "" {
		t.Fatalf("PublicAPIKey()=%q want empty", got)
	}
	if cfg.ResponseStoreTTL != 720 {
		t.Fatalf("ResponseStoreTTL=%d want=720", cfg.ResponseStoreTTL)
	}
	if cfg.DeploymentReplicas != 1 || cfg.DeploymentCluster != "orchids" {
		t.Fatalf("deployment defaults = replicas %d cluster %q", cfg.DeploymentReplicas, cfg.DeploymentCluster)
	}
	if cfg.MediaDir != "data"+string(filepath.Separator)+"tmp" {
		t.Fatalf("MediaDir=%q", cfg.MediaDir)
	}
}

func TestApplyDefaultsGeneratesRandomPassword(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)

	if cfg.AdminPass == "" {
		t.Fatal("AdminPass should not be empty after ApplyDefaults")
	}
	if cfg.AdminPass == "admin123" {
		t.Fatal("AdminPass should not be the old default 'admin123'")
	}
	if len(cfg.AdminPass) < 16 {
		t.Fatalf("AdminPass too short: got %d chars, want at least 16", len(cfg.AdminPass))
	}

	// Verify each call generates a different password.
	var cfg2 Config
	ApplyDefaults(&cfg2)
	if cfg.AdminPass == cfg2.AdminPass {
		t.Fatal("Two calls to ApplyDefaults should generate different passwords")
	}
}

func TestApplyHardcodedOverridesValues(t *testing.T) {
	cfg := Config{
		ContextMaxTokens: 999,
		MaxRetries:       999,
		RequestTimeout:   999,
	}
	ApplyHardcoded(&cfg)

	if cfg.ContextMaxTokens != 100000 {
		t.Fatalf("ContextMaxTokens=%d want=100000", cfg.ContextMaxTokens)
	}
	if cfg.MaxRetries != 20 {
		t.Fatalf("MaxRetries=%d want bounded maximum 20", cfg.MaxRetries)
	}
	if cfg.RequestTimeout != 999 {
		t.Fatalf("RequestTimeout=%d want configured 999", cfg.RequestTimeout)
	}
	if cfg.ConcurrencyTimeout != cfg.RequestTimeout {
		t.Fatalf("ConcurrencyTimeout=%d want RequestTimeout=%d", cfg.ConcurrencyTimeout, cfg.RequestTimeout)
	}
	if cfg.UpstreamMode != "ws" {
		t.Fatalf("UpstreamMode=%q want=ws", cfg.UpstreamMode)
	}
}

func TestApplyDefaultsPreservesConfigurableFields(t *testing.T) {
	cfg := Config{
		Port:               "8080",
		AdminUser:          "myuser",
		AdminPass:          "mypass",
		AdminPath:          "/myadmin",
		RedisAddr:          "redis:6380",
		DeploymentReplicas: 3,
		DeploymentInstance: "replica-a",
		DeploymentCluster:  "cluster-a",
		SharedMedia:        true,
		MediaDir:           "/srv/orchids-media",
	}
	ApplyDefaults(&cfg)

	if cfg.Port != "8080" {
		t.Fatalf("Port=%q want=8080", cfg.Port)
	}
	if cfg.AdminUser != "myuser" {
		t.Fatalf("AdminUser=%q want=myuser", cfg.AdminUser)
	}
	if cfg.AdminPass != "mypass" {
		t.Fatalf("AdminPass=%q want=mypass", cfg.AdminPass)
	}
	if cfg.AdminPath != "/myadmin" {
		t.Fatalf("AdminPath=%q want=/myadmin", cfg.AdminPath)
	}
	if cfg.RedisAddr != "redis:6380" {
		t.Fatalf("RedisAddr=%q want=redis:6380", cfg.RedisAddr)
	}
	if cfg.DeploymentReplicas != 3 || cfg.DeploymentInstance != "replica-a" || cfg.DeploymentCluster != "cluster-a" || !cfg.SharedMedia || cfg.MediaDir != "/srv/orchids-media" {
		t.Fatalf("deployment fields were not preserved: %+v", cfg)
	}
}
