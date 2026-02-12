package config

import (
	"bufio"
	"bytes"
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
	Port                      string   `json:"port"`
	DebugEnabled              bool     `json:"debug_enabled"`
	SessionID                 string   `json:"session_id"`
	ClientCookie              string   `json:"client_cookie"`
	SessionCookie             string   `json:"session_cookie"`
	ClientUat                 string   `json:"client_uat"`
	ProjectID                 string   `json:"project_id"`
	UserID                    string   `json:"user_id"`
	AgentMode                 string   `json:"agent_mode"`
	Email                     string   `json:"email"`
	AdminUser                 string   `json:"admin_user"`
	AdminPass                 string   `json:"admin_pass"`
	AdminPath                 string   `json:"admin_path"`
	DebugLogSSE               bool     `json:"debug_log_sse"`
	SuppressThinking          bool     `json:"suppress_thinking"`
	OutputTokenMode           string   `json:"output_token_mode"`
	StoreMode                 string   `json:"store_mode"`
	RedisAddr                 string   `json:"redis_addr"`
	RedisPassword             string   `json:"redis_password"`
	RedisDB                   int      `json:"redis_db"`
	RedisPrefix               string   `json:"redis_prefix"`
	SummaryCacheMode          string   `json:"summary_cache_mode"`
	SummaryCacheSize          int      `json:"summary_cache_size"`
	SummaryCacheTTLSeconds    int      `json:"summary_cache_ttl_seconds"`
	SummaryCacheLog           bool     `json:"summary_cache_log"`
	SummaryCacheRedisAddr     string   `json:"summary_cache_redis_addr"`
	SummaryCacheRedisPass     string   `json:"summary_cache_redis_password"`
	SummaryCacheRedisDB       int      `json:"summary_cache_redis_db"`
	SummaryCacheRedisPrefix   string   `json:"summary_cache_redis_prefix"`
	ContextMaxTokens          int      `json:"context_max_tokens"`
	ContextSummaryMaxTokens   int      `json:"context_summary_max_tokens"`
	ContextKeepTurns          int      `json:"context_keep_turns"`
	UpstreamURL               string   `json:"upstream_url"`
	UpstreamToken             string   `json:"upstream_token"`
	UpstreamMode              string   `json:"upstream_mode"`
	OrchidsAPIBaseURL         string   `json:"orchids_api_base_url"`
	OrchidsWSURL              string   `json:"orchids_ws_url"`
	OrchidsAPIVersion         string   `json:"orchids_api_version"`
	OrchidsImpl               string   `json:"orchids_impl"`
	OrchidsAllowRunCommand    bool     `json:"orchids_allow_run_command"`
	OrchidsRunAllowlist       []string `json:"orchids_run_allowlist"`
	OrchidsCCEntrypointMode   string   `json:"orchids_cc_entrypoint_mode"`
	OrchidsFSIgnore           []string `json:"orchids_fs_ignore"`
	WarpDisableTools          *bool    `json:"warp_disable_tools"`
	WarpMaxToolResults        int      `json:"warp_max_tool_results"`
	WarpMaxHistoryMessages    int      `json:"warp_max_history_messages"`
	WarpSplitToolResults      bool     `json:"warp_split_tool_results"`
	OrchidsMaxToolResults     int      `json:"orchids_max_tool_results"`
	OrchidsMaxHistoryMessages int      `json:"orchids_max_history_messages"`

	// New fields for UI
	AdminToken           string `json:"admin_token"`
	MaxRetries           int    `json:"max_retries"`
	RetryDelay           int    `json:"retry_delay"`
	AccountSwitchCount   int    `json:"account_switch_count"`
	RequestTimeout       int    `json:"request_timeout"`
	Retry429Interval     int    `json:"retry_429_interval"`
	TokenRefreshInterval int    `json:"token_refresh_interval"`
	AutoRefreshToken     bool   `json:"auto_refresh_token"`
	OutputTokenCount     bool   `json:"output_token_count"`
	CacheTokenCount      bool   `json:"cache_token_count"`
	CacheTTL             int    `json:"cache_ttl"`
	CacheStrategy        string `json:"cache_strategy"`
	LoadBalancerCacheTTL int    `json:"load_balancer_cache_ttl"`
	ConcurrencyLimit     int    `json:"concurrency_limit"`
	ConcurrencyTimeout   int    `json:"concurrency_timeout"`
	AdaptiveTimeout      bool   `json:"adaptive_timeout"`

	// Proxy Configuration
	ProxyHTTP   string   `json:"proxy_http"`
	ProxyHTTPS  string   `json:"proxy_https"`
	ProxyUser   string   `json:"proxy_user"`
	ProxyPass   string   `json:"proxy_pass"`
	ProxyBypass []string `json:"proxy_bypass"`

	// Auto Registration
	AutoRegEnabled   bool   `json:"auto_reg_enabled"`
	AutoRegThreshold int    `json:"auto_reg_threshold"`
	AutoRegScript    string `json:"auto_reg_script"`
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
		cfg.AdminPass = "admin123"
		slog.Warn("使用默认管理员密码 admin123，请在配置文件中设置 admin_pass")
	}
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	if cfg.OutputTokenMode == "" {
		cfg.OutputTokenMode = "final"
	}
	if cfg.UpstreamMode == "" {
		cfg.UpstreamMode = "sse"
	}
	if cfg.StoreMode == "" {
		cfg.StoreMode = "redis"
	}
	if cfg.RedisPrefix == "" {
		cfg.RedisPrefix = "orchids:"
	}
	if cfg.SummaryCacheMode == "" {
		if strings.ToLower(strings.TrimSpace(cfg.StoreMode)) == "redis" {
			cfg.SummaryCacheMode = "redis"
		} else {
			cfg.SummaryCacheMode = "memory"
		}
	}
	if strings.ToLower(strings.TrimSpace(cfg.SummaryCacheMode)) == "redis" {
		if cfg.SummaryCacheRedisAddr == "" {
			cfg.SummaryCacheRedisAddr = cfg.RedisAddr
		}
		if cfg.SummaryCacheRedisPass == "" {
			cfg.SummaryCacheRedisPass = cfg.RedisPassword
		}
	}
	if cfg.SummaryCacheSize == 0 {
		cfg.SummaryCacheSize = 256
	}
	if cfg.SummaryCacheTTLSeconds == 0 {
		cfg.SummaryCacheTTLSeconds = 3600
	}
	if cfg.SummaryCacheRedisPrefix == "" {
		cfg.SummaryCacheRedisPrefix = "orchids:summary:"
	}
	if cfg.ContextMaxTokens == 0 {
		cfg.ContextMaxTokens = 8000
	}
	if cfg.ContextSummaryMaxTokens == 0 {
		cfg.ContextSummaryMaxTokens = 800
	}
	if cfg.ContextKeepTurns == 0 {
		cfg.ContextKeepTurns = 6
	}
	if cfg.OrchidsAPIBaseURL == "" {
		cfg.OrchidsAPIBaseURL = "https://orchids-server.calmstone-6964e08a.westeurope.azurecontainerapps.io"
	}
	if cfg.OrchidsWSURL == "" {
		cfg.OrchidsWSURL = "wss://orchids-v2-alpha-108292236521.europe-west1.run.app/agent/ws/coding-agent"
	}
	if cfg.OrchidsAPIVersion == "" {
		cfg.OrchidsAPIVersion = "2"
	}
	// Only AIClient mode is supported.
	if cfg.OrchidsImpl == "" {
		cfg.OrchidsImpl = "aiclient"
	}
	if len(cfg.OrchidsRunAllowlist) == 0 {
		cfg.OrchidsRunAllowlist = []string{"pwd", "ls", "find"}
	}
	if cfg.OrchidsCCEntrypointMode == "" {
		cfg.OrchidsCCEntrypointMode = "auto"
	}
	if len(cfg.OrchidsFSIgnore) == 0 {
		cfg.OrchidsFSIgnore = []string{"debug-logs", "data", ".claude"}
	}

	if cfg.WarpDisableTools == nil {
		v := false
		cfg.WarpDisableTools = &v
	}
	if cfg.WarpMaxToolResults == 0 {
		cfg.WarpMaxToolResults = 10
	}
	if cfg.WarpMaxHistoryMessages == 0 {
		cfg.WarpMaxHistoryMessages = 20
	}
	if cfg.OrchidsMaxToolResults == 0 {
		cfg.OrchidsMaxToolResults = 10
	}
	if cfg.OrchidsMaxHistoryMessages == 0 {
		cfg.OrchidsMaxHistoryMessages = 20
	}

	// New defaults
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = 1000
	}
	if cfg.AccountSwitchCount == 0 {
		cfg.AccountSwitchCount = 5
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 600
	}
	if cfg.Retry429Interval == 0 {
		cfg.Retry429Interval = 60
	}
	if cfg.TokenRefreshInterval == 0 {
		cfg.TokenRefreshInterval = 1
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5
	}
	if cfg.CacheStrategy == "" {
		cfg.CacheStrategy = "mixed"
	}
	if cfg.LoadBalancerCacheTTL == 0 {
		cfg.LoadBalancerCacheTTL = 5
	}
	if cfg.ConcurrencyLimit == 0 {
		cfg.ConcurrencyLimit = 100
	}
	if cfg.ConcurrencyTimeout == 0 {
		cfg.ConcurrencyTimeout = 300
	}

	// Auto Reg defaults
	if cfg.AutoRegThreshold == 0 {
		cfg.AutoRegThreshold = 5
	}
	if cfg.AutoRegScript == "" {
		cfg.AutoRegScript = "scripts/autoreg.py"
	}
}

func (c *Config) GetCookies() string {
	if strings.TrimSpace(c.SessionCookie) != "" {
		return "__client=" + c.ClientCookie + "; __session=" + c.SessionCookie
	}
	return "__client=" + c.ClientCookie + "; __client_uat=" + c.ClientUat
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
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
