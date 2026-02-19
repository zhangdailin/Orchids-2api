package config

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	// ── Configurable fields (read from config.json / Redis) ──
	Port          string `json:"port"`
	DebugEnabled  bool   `json:"debug_enabled"`
	AdminUser     string `json:"admin_user"`
	AdminPass     string `json:"admin_pass"`
	AdminPath     string `json:"admin_path"`
	AdminToken    string `json:"admin_token"`
	StoreMode     string `json:"store_mode"`
	RedisAddr     string `json:"redis_addr"`
	RedisPassword string `json:"redis_password"`
	RedisDB       int    `json:"redis_db"`
	RedisPrefix   string `json:"redis_prefix"`
	CacheTokenCount bool   `json:"cache_token_count"`
	CacheTTL        int    `json:"cache_ttl"`
	CacheStrategy   string `json:"cache_strategy"`

	// ── Per-client state (used by orchids client, not configurable) ──
	SessionID     string `json:"-"`
	ClientCookie  string `json:"-"`
	SessionCookie string `json:"-"`
	ClientUat     string `json:"-"`
	ProjectID     string `json:"-"`
	UserID        string `json:"-"`
	AgentMode     string `json:"-"`
	Email         string `json:"-"`

	// ── Hardcoded fields (set unconditionally by ApplyHardcoded) ──
	DebugLogSSE               bool     `json:"-"`
	SuppressThinking          bool     `json:"-"`
	OutputTokenMode           string   `json:"-"`
	ContextMaxTokens          int      `json:"-"`
	ContextSummaryMaxTokens   int      `json:"-"`
	ContextKeepTurns          int      `json:"-"`
	UpstreamURL               string   `json:"-"`
	UpstreamToken             string   `json:"-"`
	UpstreamMode              string   `json:"-"`
	OrchidsAPIBaseURL         string   `json:"-"`
	OrchidsWSURL              string   `json:"-"`
	OrchidsAPIVersion         string   `json:"-"`
	OrchidsAllowRunCommand    bool     `json:"-"`
	OrchidsRunAllowlist       []string `json:"-"`
	OrchidsCCEntrypointMode   string   `json:"-"`
	OrchidsFSIgnore           []string `json:"-"`
	GrokAPIBaseURL            string   `json:"-"`
	GrokUserAgent             string   `json:"-"`
	GrokCFClearance           string   `json:"-"`
	GrokCFBM                  string   `json:"-"`
	GrokBaseProxyURL          string   `json:"-"`
	GrokAssetProxyURL         string   `json:"-"`
	GrokUseUTLS               bool     `json:"-"`
	WarpDisableTools          *bool    `json:"-"`
	WarpMaxToolResults        int      `json:"-"`
	WarpMaxHistoryMessages    int      `json:"-"`
	WarpSplitToolResults      bool     `json:"-"`
	OrchidsMaxToolResults     int      `json:"-"`
	OrchidsMaxHistoryMessages int      `json:"-"`
	Stream                    *bool    `json:"-"`
	ImageNSFW                 *bool    `json:"-"`
	ImageFinalMinBytes        int      `json:"-"`
	ImageMediumMinBytes       int      `json:"-"`
	MaxRetries                int      `json:"-"`
	RetryDelay                int      `json:"-"`
	AccountSwitchCount        int      `json:"-"`
	RequestTimeout            int      `json:"-"`
	Retry429Interval          int      `json:"-"`
	TokenRefreshInterval      int      `json:"-"`
	AutoRefreshToken          bool     `json:"-"`
	OutputTokenCount          bool     `json:"-"`
	LoadBalancerCacheTTL      int      `json:"-"`
	ConcurrencyLimit          int      `json:"-"`
	ConcurrencyTimeout        int      `json:"-"`
	AdaptiveTimeout           bool     `json:"-"`
	ProxyHTTP                 string   `json:"proxy_http"`
	ProxyHTTPS                string   `json:"proxy_https"`
	ProxyUser                 string   `json:"proxy_user"`
	ProxyPass                 string   `json:"proxy_pass"`
	ProxyBypass               []string `json:"proxy_bypass"`
	AutoRegEnabled            bool     `json:"-"`
	AutoRegThreshold          int      `json:"-"`
	AutoRegScript             string   `json:"-"`
	PublicKey                 string   `json:"-"`
	PublicEnabled             *bool    `json:"-"`
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
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5
	}
	if strings.TrimSpace(cfg.CacheStrategy) == "" {
		cfg.CacheStrategy = "mix"
	}
	// Always apply hardcoded values
	ApplyHardcoded(cfg)
}

// ApplyHardcoded unconditionally sets all non-configurable fields to their
// fixed values. Call this after any JSON decode (config file, Redis, API)
// to ensure these values cannot be overridden.
func ApplyHardcoded(cfg *Config) {
	cfg.OutputTokenMode = "final"
	cfg.UpstreamMode = "sse"
	cfg.ContextMaxTokens = 100000
	cfg.ContextSummaryMaxTokens = 800
	cfg.ContextKeepTurns = 6
	cfg.OrchidsAPIBaseURL = "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io"
	cfg.OrchidsWSURL = "wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent"
	cfg.OrchidsAPIVersion = "2"
	cfg.OrchidsAllowRunCommand = true
	cfg.OrchidsRunAllowlist = []string{"*"}
	cfg.OrchidsCCEntrypointMode = "auto"
	cfg.OrchidsFSIgnore = []string{"debug-logs", "data", ".claude"}
	cfg.GrokAPIBaseURL = "https://grok.com"
	cfg.GrokUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	v := false
	cfg.WarpDisableTools = &v
	cfg.WarpMaxToolResults = 10
	cfg.WarpMaxHistoryMessages = 20
	cfg.OrchidsMaxToolResults = 10
	cfg.OrchidsMaxHistoryMessages = 20
	vTrue := true
	cfg.Stream = &vTrue
	cfg.ImageNSFW = &vTrue
	cfg.ImageFinalMinBytes = 100000
	cfg.ImageMediumMinBytes = 30000
	cfg.MaxRetries = 3
	cfg.RetryDelay = 1000
	cfg.AccountSwitchCount = 5
	cfg.RequestTimeout = 600
	cfg.Retry429Interval = 60
	cfg.TokenRefreshInterval = 1
	cfg.AutoRefreshToken = true
	cfg.LoadBalancerCacheTTL = 5
	cfg.ConcurrencyLimit = 100
	cfg.ConcurrencyTimeout = 300
	cfg.AdaptiveTimeout = true
	cfg.AutoRegThreshold = 5
	cfg.AutoRegScript = "scripts/autoreg.py"
	cfg.DebugLogSSE = true
}

func (c *Config) ChatDefaultStream() bool {
	if c == nil || c.Stream == nil {
		return true
	}
	return *c.Stream
}

func (c *Config) PublicImagineNSFW() bool {
	if c == nil || c.ImageNSFW == nil {
		return true
	}
	return *c.ImageNSFW
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
	if c == nil || c.PublicEnabled == nil {
		return false
	}
	return *c.PublicEnabled
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func generateRandomPassword(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:length], nil
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
