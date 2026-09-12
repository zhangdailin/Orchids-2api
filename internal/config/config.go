package config

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
)

type Config struct {
	// ── Configurable fields (read from config.json / Redis) ──
	Port               string   `json:"port"`
	DebugEnabled       bool     `json:"debug_enabled"`
	VerboseDiagnostics bool     `json:"verbose_diagnostics,omitempty"`
	AdminUser          string   `json:"admin_user"`
	AdminPass          string   `json:"admin_pass"`
	AdminPath          string   `json:"admin_path"`
	AdminToken         string   `json:"admin_token"`
	InferenceAuth      *bool    `json:"inference_auth_enabled,omitempty"`
	CredentialKeyFile  string   `json:"credential_encryption_key_file,omitempty"`
	ResponseStoreTTL   int      `json:"response_store_ttl_hours,omitempty"`
	TrustedProxies     []string `json:"trusted_proxies,omitempty"`
	StoreMode          string   `json:"store_mode"`
	RedisAddr          string   `json:"redis_addr"`
	RedisPassword      string   `json:"redis_password"`
	RedisDB            int      `json:"redis_db"`
	RedisPrefix        string   `json:"redis_prefix"`
	DeploymentReplicas int      `json:"deployment_replicas,omitempty"`
	DeploymentInstance string   `json:"deployment_instance_id,omitempty"`
	DeploymentCluster  string   `json:"deployment_cluster_id,omitempty"`
	SharedMedia        bool     `json:"shared_media,omitempty"`
	MediaDir           string   `json:"media_dir,omitempty"`
	CacheTokenCount    bool     `json:"cache_token_count"`
	CacheTTL           int      `json:"cache_ttl"`
	CacheStrategy      string   `json:"cache_strategy"`
	EnableTokenCache   bool     `json:"enable_token_cache"`
	TokenCacheTTL      int      `json:"token_cache_ttl"`
	TokenCacheStrategy string   `json:"token_cache_strategy"`

	// ── Hardcoded fields (set unconditionally by ApplyHardcoded) ──
	DebugLogSSE             bool   `json:"-"`
	SuppressThinking        bool   `json:"-"`
	ContextMaxTokens        int    `json:"-"`
	ContextSummaryMaxTokens int    `json:"-"`
	ContextKeepTurns        int    `json:"-"`
	UpstreamURL             string `json:"-"`
	UpstreamToken           string `json:"-"`
	UpstreamMode            string `json:"-"`
	GrokAPIBaseURL          string `json:"-"`
	GrokUserAgent           string `json:"-"`
	GrokCFClearance         string `json:"-"`
	GrokCFBM                string `json:"-"`
	GrokStatsigID           string `json:"grok_statsig_id,omitempty"`
	GrokConfigCFClearance   string `json:"grok_cf_clearance,omitempty"`
	GrokConfigCFBM          string `json:"grok_cf_bm,omitempty"`
	GrokBaseProxyURL        string `json:"-"`
	GrokAssetProxyURL       string `json:"-"`
	GrokTemporary           *bool  `json:"grok_temporary,omitempty"`
	GrokDisableMemory       *bool  `json:"grok_disable_memory,omitempty"`
	GrokCustomInstruction   string `json:"grok_custom_instruction,omitempty"`

	// ── WorkBuddy international backend (www.workbuddy.ai) ──
	// Overridable for self-hosted regional deployments and for tests that need a
	// stubbed upstream. Empty means the production international host.
	WorkBuddyBaseURL string `json:"workbuddy_base_url,omitempty"`

	// ── Grok Build CLI (cli-chat-proxy.grok.com) OAuth upstream ──
	// These fields are configurable via config.json / Redis and are deliberately
	// NOT written into ApplyHardcoded, so they survive a persistConfig round trip.
	GrokCLIBaseURL          string   `json:"grok_cli_base_url,omitempty"`
	GrokCLIFallbackBaseURL  string   `json:"grok_cli_fallback_base_url,omitempty"`
	GrokConsoleBaseURL      string   `json:"grok_console_base_url,omitempty"`
	GrokCLIUserAgent        string   `json:"grok_cli_user_agent,omitempty"`
	GrokCLIClientVersion    string   `json:"grok_cli_client_version,omitempty"`
	GrokCLIClientIdentifier string   `json:"grok_cli_client_identifier,omitempty"`
	GrokCLIOAuthClientID    string   `json:"grok_cli_oauth_client_id,omitempty"`
	GrokCLIOAuthDeviceURL   string   `json:"grok_cli_oauth_device_url,omitempty"`
	GrokCLIOAuthTokenURL    string   `json:"grok_cli_oauth_token_url,omitempty"`
	GrokCLIModelIDs         []string `json:"grok_cli_model_ids,omitempty"`
	GrokWebRPS              float64  `json:"grok_web_rps,omitempty"`
	GrokConsoleRPS          float64  `json:"grok_console_rps,omitempty"`
	GrokBuildRPS            float64  `json:"grok_build_rps,omitempty"`
	GrokWebTimeout          int      `json:"grok_web_timeout_seconds,omitempty"`
	GrokConsoleTimeout      int      `json:"grok_console_timeout_seconds,omitempty"`
	GrokBuildTimeout        int      `json:"grok_build_timeout_seconds,omitempty"`
	GrokStreamIdleSeconds   int      `json:"grok_stream_idle_seconds,omitempty"`

	// ── Channel probes (synthetic reachability checks) ──
	// The model a probe asks for. Empty means the built-in cheap default; a
	// deployment on a plan that does not serve the default can point probes at a
	// model it does serve instead of recording a permanent false outage.
	GrokProbeModel string `json:"grok_probe_model,omitempty"`

	// ── Grok egress (proxy pool + FlareSolverr + clearance) ──
	GrokEgressEnabled          bool               `json:"grok_egress_enabled,omitempty"`
	GrokEgressNodes            []EgressNodeConfig `json:"grok_egress_nodes,omitempty"`
	GrokFlareSolverrURL        string             `json:"grok_flaresolverr_url,omitempty"`
	GrokClearanceMode          string             `json:"grok_clearance_mode,omitempty"`             // "manual"|"flaresolverr"
	GrokClearanceRefreshInterv int                `json:"grok_clearance_refresh_interval,omitempty"` // seconds

	// ── Upstream fidelity (defaults preserve client content verbatim) ──
	// A relay gateway forwards client messages without rewriting content.
	// This field is NOT written into ApplyHardcoded, so it survives a
	// persistConfig round trip.
	WarpDisableTools       *bool    `json:"-"`
	WarpMaxToolResults     int      `json:"-"`
	WarpMaxHistoryMessages int      `json:"-"`
	Stream                 *bool    `json:"-"`
	ImageNSFW              *bool    `json:"-"`
	ImageFinalMinBytes     int      `json:"-"`
	ImageMediumMinBytes    int      `json:"-"`
	MaxRetries             int      `json:"max_retries,omitempty"`
	RetryDelay             int      `json:"retry_delay,omitempty"`
	AccountSwitchCount     int      `json:"account_switch_count,omitempty"`
	RequestTimeout         int      `json:"request_timeout,omitempty"`
	Retry429Interval       int      `json:"retry_429_interval,omitempty"`
	TokenRefreshInterval   int      `json:"-"`
	AutoRefreshToken       bool     `json:"-"`
	LoadBalancerCacheTTL   int      `json:"-"`
	ConcurrencyLimit       int      `json:"-"`
	ConcurrencyTimeout     int      `json:"concurrency_timeout,omitempty"`
	AdaptiveTimeout        bool     `json:"-"`
	ProxyURL               string   `json:"proxy_url"`
	ProxyHTTP              string   `json:"proxy_http"`
	ProxyHTTPS             string   `json:"proxy_https"`
	ProxyUser              string   `json:"proxy_user"`
	ProxyPass              string   `json:"proxy_pass"`
	ProxyBypass            []string `json:"proxy_bypass"`
	PublicKey              string   `json:"-"`
	PublicEnabled          *bool    `json:"-"`
}

// EgressNodeConfig describes one egress exit node for the Grok proxy pool.
// Defined in the config package (not grok/egress) to avoid an import cycle.
type EgressNodeConfig struct {
	Name    string `json:"name"`
	URL     string `json:"url"`    // proxy address http/socks5; empty = direct
	Weight  int    `json:"weight"` // weight for weighted round-robin; <=0 = 1
	Scope   string `json:"scope"`  // "app_chat"|"console"|"cli"|"all"
	Proxied bool   `json:"proxied"`
}

func Load(path string) (*Config, string, error) {
	resolvedPath, err := resolveConfigPath(path)
	if err != nil {
		return nil, "", err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read config: %w", err)
	}

	cfg := Config{}
	ext := strings.ToLower(filepath.Ext(resolvedPath))
	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config json: %w", err)
		}
	case ".yaml", ".yml":
		m, err := parseYAMLFlat(data)
		if err != nil {
			return nil, "", err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, "", fmt.Errorf("failed to normalize yaml: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config yaml: %w", err)
		}
	default:
		return nil, "", fmt.Errorf("unsupported config extension: %s", ext)
	}

	ApplyDefaults(&cfg)
	return &cfg, resolvedPath, nil
}

func resolveConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}

	candidates := []string{"config.json", "config.yaml", "config.yml"}
	for _, name := range candidates {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	return "", errors.New("config.json/config.yaml/config.yml not found")
}

func ApplyDefaults(cfg *Config) {
	if cfg.Port == "" {
		cfg.Port = "3002"
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	if cfg.AdminPass == "" {
		generated, err := generateRandomPassword(16)
		if err != nil {
			slog.Error("无法生成随机密码", "error", err)
			os.Exit(1)
		}
		cfg.AdminPass = generated
		slog.Warn("未设置 admin_pass，已自动生成随机密码，请在配置文件中设置 admin_pass",
			"generated_password", generated)
	}
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	if cfg.StoreMode == "" {
		cfg.StoreMode = "redis"
	}
	if cfg.RedisPrefix == "" {
		cfg.RedisPrefix = "orchids:"
	}
	if cfg.DeploymentReplicas <= 0 {
		cfg.DeploymentReplicas = 1
	}
	if strings.TrimSpace(cfg.DeploymentCluster) == "" {
		cfg.DeploymentCluster = "orchids"
	}
	if strings.TrimSpace(cfg.MediaDir) == "" {
		cfg.MediaDir = filepath.Join("data", "tmp")
	}
	if strings.TrimSpace(cfg.CredentialKeyFile) == "" {
		cfg.CredentialKeyFile = filepath.Join("data", "credential.key")
	}
	if cfg.ResponseStoreTTL <= 0 {
		cfg.ResponseStoreTTL = 30 * 24
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5
	}
	if strings.TrimSpace(cfg.CacheStrategy) == "" {
		cfg.CacheStrategy = "mix"
	}
	if cfg.TokenCacheTTL <= 0 {
		cfg.TokenCacheTTL = 300
	}
	if strings.TrimSpace(cfg.TokenCacheStrategy) == "" {
		cfg.TokenCacheStrategy = "1"
	}
	// Fidelity default: preserve client content verbatim unless explicitly
	// configured otherwise ("auto"/"strip" re-enable cc_entrypoint handling).
	// Always apply hardcoded values
	ApplyHardcoded(cfg)
}

// ApplyHardcoded sets fixed fields and supplies bounded defaults for runtime
// retry/deadline settings. Configured runtime values survive file/Redis/API
// round trips; protocol constants remain non-configurable.
func ApplyHardcoded(cfg *Config) {
	cfg.UpstreamMode = "ws"
	cfg.ContextMaxTokens = 100000
	cfg.ContextSummaryMaxTokens = 800
	cfg.ContextKeepTurns = 6
	cfg.GrokAPIBaseURL = "https://grok.com"
	cfg.GrokUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	v := false
	cfg.WarpDisableTools = &v
	cfg.WarpMaxToolResults = 10
	cfg.WarpMaxHistoryMessages = 20
	vTrue := true
	cfg.Stream = &vTrue
	cfg.ImageNSFW = &vTrue
	cfg.PublicEnabled = &vTrue
	cfg.ImageFinalMinBytes = 100000
	cfg.ImageMediumMinBytes = 30000
	cfg.MaxRetries = boundedDefault(cfg.MaxRetries, 3, 20)
	cfg.RetryDelay = boundedDefault(cfg.RetryDelay, 1000, 60000)
	cfg.AccountSwitchCount = boundedDefault(cfg.AccountSwitchCount, 5, 20)
	cfg.RequestTimeout = boundedDefault(cfg.RequestTimeout, 600, 86400)
	cfg.Retry429Interval = boundedDefault(cfg.Retry429Interval, 60, 3600)
	cfg.TokenRefreshInterval = 1
	cfg.AutoRefreshToken = true
	cfg.LoadBalancerCacheTTL = 5
	cfg.ConcurrencyLimit = 100
	cfg.ConcurrencyTimeout = boundedDefault(cfg.ConcurrencyTimeout, cfg.RequestTimeout, 86400)
	cfg.AdaptiveTimeout = true
	cfg.DebugLogSSE = cfg.DebugEnabled
}

func (c *Config) VerboseDiagnosticsEnabled() bool {
	return c != nil && c.DebugEnabled && c.VerboseDiagnostics
}

func (c *Config) ChatDefaultStream() bool {
	return c == nil || c.Stream == nil || *c.Stream
}

func (c *Config) GrokChatTemporary() bool {
	return c == nil || c.GrokTemporary == nil || *c.GrokTemporary
}

func (c *Config) GrokChatDisableMemory(defaultValue bool) bool {
	if c == nil || c.GrokDisableMemory == nil {
		return defaultValue
	}
	return *c.GrokDisableMemory
}

func (c *Config) GrokChatCustomInstruction() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GrokCustomInstruction)
}

// GrokCLIBaseURLOrDefault returns the Build CLI proxy base URL, defaulting to
// the official gateway.
func (c *Config) GrokCLIBaseURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.GrokCLIBaseURL), "/")
	}
	return "https://cli-chat-proxy.grok.com/v1"
}

func (c *Config) GrokCLIFallbackBaseURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIFallbackBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.GrokCLIFallbackBaseURL), "/")
	}
	return "https://api.x.ai/v1"
}

// GrokConsoleBaseURLOrDefault returns the Console v1 API base used by DPoP
// authenticated text, media, and voice requests.
func (c *Config) GrokConsoleBaseURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokConsoleBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.GrokConsoleBaseURL), "/")
	}
	return "https://console.x.ai/v1"
}

// GrokCLIOAuthClientIDOrDefault returns the xAI OAuth client ID used for Build
// token refresh, defaulting to the official CLI client.
func (c *Config) GrokCLIOAuthClientIDOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthClientID) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthClientID)
	}
	return "b1a00492-073a-47ea-816f-4c329264a828"
}

// GrokCLIOAuthDeviceURLOrDefault returns the xAI OAuth device-authorization
// endpoint used by the official Grok Build CLI.
func (c *Config) GrokCLIOAuthDeviceURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthDeviceURL) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthDeviceURL)
	}
	return "https://auth.x.ai/oauth2/device/code"
}

// GrokCLIOAuthTokenURLOrDefault returns the xAI OAuth token endpoint.
func (c *Config) GrokCLIOAuthTokenURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthTokenURL) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthTokenURL)
	}
	return "https://auth.x.ai/oauth2/token"
}

// GrokCLIUserAgentOrDefault returns the CLI user agent stamped on Build
// requests. A fixed CLI identity (not the browser UA) is required upstream.
func (c *Config) GrokCLIUserAgentOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIUserAgent) != "" {
		return strings.TrimSpace(c.GrokCLIUserAgent)
	}
	return "grok-shell/1.0.4 (linux; x86_64)"
}

// GrokCLIClientVersionOrDefault returns the x-grok-client-version header value.
func (c *Config) GrokCLIClientVersionOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIClientVersion) != "" {
		return strings.TrimSpace(c.GrokCLIClientVersion)
	}
	return "1.0.4"
}

// GrokCLIClientIdentifierOrDefault returns the x-grok-client-identifier header.
func (c *Config) GrokCLIClientIdentifierOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIClientIdentifier) != "" {
		return strings.TrimSpace(c.GrokCLIClientIdentifier)
	}
	return "grok-shell"
}

// GrokClearanceRefreshIntervalOrDefault returns the clearance auto-refresh
// interval in seconds (default 600s).
func (c *Config) GrokClearanceRefreshIntervalOrDefault() int {
	if c != nil && c.GrokClearanceRefreshInterv > 0 {
		return c.GrokClearanceRefreshInterv
	}
	return 600
}

// GrokClearanceModeOrDefault returns "manual" when FlareSolverr is not usable.
func (c *Config) GrokClearanceModeOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokClearanceMode) != "" {
		return strings.TrimSpace(c.GrokClearanceMode)
	}
	return "manual"
}

// GrokModelIsCLI reports whether the given model ID is routed to the Build CLI
// upstream via the GrokCLIModelIDs list.
func (c *Config) GrokModelIsCLI(modelID string) bool {
	if c == nil {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(modelID))
	for _, id := range c.GrokCLIModelIDs {
		if strings.ToLower(strings.TrimSpace(id)) == target {
			return true
		}
	}
	return false
}

func (c *Config) PublicImagineNSFW() bool {
	return c == nil || c.ImageNSFW == nil || *c.ImageNSFW
}

func (c *Config) PublicImagineFinalMinBytes() int {
	if c == nil || c.ImageFinalMinBytes <= 0 {
		return 100000
	}
	return c.ImageFinalMinBytes
}

func (c *Config) PublicImagineMediumMinBytes() int {
	if c == nil || c.ImageMediumMinBytes <= 0 {
		return 30000
	}
	return c.ImageMediumMinBytes
}

func (c *Config) PublicAPIKey() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.PublicKey)
}

func (c *Config) PublicAPIEnabled() bool {
	return c != nil && c.PublicEnabled != nil && *c.PublicEnabled
}

// InferenceAuthEnabled reports whether model and inference endpoints require
// a managed API key. Authentication is enabled by default; trusted upstream
// gateways can explicitly opt out with inference_auth_enabled=false.
func (c *Config) InferenceAuthEnabled() bool {
	return c == nil || c.InferenceAuth == nil || *c.InferenceAuth
}

func generateRandomPassword(length int) (string, error) {
	// hex encoding doubles the length, so we only need half the bytes
	byteLen := (length + 1) / 2
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	encoded := hex.EncodeToString(b)
	if len(encoded) > length {
		encoded = encoded[:length]
	}
	return encoded, nil
}

func parseYAMLFlat(data []byte) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Only strip inline comments where # is preceded by whitespace,
		// to avoid corrupting values containing # (hex colors, URLs, etc.)
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		} else if idx := strings.Index(line, "\t#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid yaml line: %q", line)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, "\"'")

		if key == "" {
			continue
		}
		if value == "" {
			out[key] = ""
			continue
		}
		if value == "true" || value == "false" {
			out[key] = value == "true"
			continue
		}
		if num, err := strconv.Atoi(value); err == nil {
			out[key] = num
			continue
		}
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
