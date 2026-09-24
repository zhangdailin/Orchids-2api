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
	if got := cfg.GrokCLIClientVersionOrDefault(); got != "1.0.40" {
		t.Fatalf("GrokCLIClientVersionOrDefault()=%q", got)
	}
	if got := cfg.GrokCLIUserAgentOrDefault(); got != "grok-shell/1.0.40 (linux; x86_64)" {
		t.Fatalf("GrokCLIUserAgentOrDefault()=%q", got)
	}
}

// A conversation binding must outlive an ordinary working session. Thirty
// minutes detached the upstream conversation between turns, which forced the
// next turn to replay the whole transcript.
func TestConfigDefaultsKeepConversationBindingsAlive(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.SessionTTLMinutes < 60 {
		t.Fatalf("SessionTTLMinutes=%d, want at least an hour", cfg.SessionTTLMinutes)
	}
}

// The stateless transcript ceiling is a transport bound, not a context policy:
// it has to stay above any single model window.
func TestConfigDefaultsLeaveTheTranscriptCeilingAboveAnyModelWindow(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.WarpStatelessHistoryMaxChars < 1<<20 {
		t.Fatalf("WarpStatelessHistoryMaxChars=%d, want at least 1 MiB", cfg.WarpStatelessHistoryMaxChars)
	}
}

// An operator's explicit values survive, and an absurd one is still bounded.
func TestConfigKeepsExplicitContextSettingsWithinBounds(t *testing.T) {
	cfg := Config{SessionTTLMinutes: 90, WarpStatelessHistoryMaxChars: 2 << 20}
	ApplyDefaults(&cfg)
	if cfg.SessionTTLMinutes != 90 {
		t.Fatalf("SessionTTLMinutes=%d, want the configured 90", cfg.SessionTTLMinutes)
	}
	if cfg.WarpStatelessHistoryMaxChars != 2<<20 {
		t.Fatalf("WarpStatelessHistoryMaxChars=%d, want the configured 2 MiB", cfg.WarpStatelessHistoryMaxChars)
	}

	over := Config{SessionTTLMinutes: 1 << 30, WarpStatelessHistoryMaxChars: 1 << 30}
	ApplyDefaults(&over)
	if over.SessionTTLMinutes > 30*24*60 {
		t.Fatalf("SessionTTLMinutes=%d is unbounded", over.SessionTTLMinutes)
	}
	if over.WarpStatelessHistoryMaxChars > 64<<20 {
		t.Fatalf("WarpStatelessHistoryMaxChars=%d is unbounded", over.WarpStatelessHistoryMaxChars)
	}
}

func TestCloneDeepCopiesReferenceFields(t *testing.T) {
	on := true
	original := &Config{
		InferenceAuth:   &on,
		TrustedProxies:  []string{"10.0.0.1"},
		GrokCLIModelIDs: []string{"grok-test"},
		GrokEgressNodes: []EgressNodeConfig{{Name: "primary", URL: "http://proxy"}},
		ProxyBypass:     []string{"localhost"},
	}

	clone := original.Clone()
	*clone.InferenceAuth = false
	clone.TrustedProxies[0] = "10.0.0.2"
	clone.GrokCLIModelIDs[0] = "changed"
	clone.GrokEgressNodes[0].Name = "changed"
	clone.ProxyBypass[0] = "example.com"

	if !*original.InferenceAuth || original.TrustedProxies[0] != "10.0.0.1" ||
		original.GrokCLIModelIDs[0] != "grok-test" || original.GrokEgressNodes[0].Name != "primary" ||
		original.ProxyBypass[0] != "localhost" {
		t.Fatalf("Clone shares mutable fields with original: %#v", original)
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
		MaxRetries:     999,
		RequestTimeout: 999,
	}
	ApplyHardcoded(&cfg)

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

// TestInferenceAuthOptOutSurvivesApplyDefaults pins the revert that restored
// legacy API key authentication: an explicit inference_auth_enabled=false is a
// deliberate operator choice and must not be overwritten by the hardcoded
// defaults applied on every file/Redis/API round trip. An absent field keeps
// the historical default of "enabled".
func TestInferenceAuthOptOutSurvivesApplyDefaults(t *testing.T) {
	disabled := false
	cfg := Config{InferenceAuth: &disabled}
	ApplyDefaults(&cfg)

	if cfg.InferenceAuth == nil {
		t.Fatal("ApplyDefaults dropped inference_auth_enabled")
	}
	if *cfg.InferenceAuth {
		t.Fatal("ApplyDefaults forced inference_auth_enabled back to true")
	}
	if !cfg.InferenceAuthEnabled() {
		t.Fatal("auth must stay required even when inference_auth_enabled=false")
	}

	var unset Config
	ApplyDefaults(&unset)
	if !unset.InferenceAuthEnabled() {
		t.Fatal("InferenceAuthEnabled()=false want=true when inference_auth_enabled is absent")
	}
}
