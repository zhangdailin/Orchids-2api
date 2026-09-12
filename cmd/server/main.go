package main

import (
	"context"
	"flag"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"orchids-api/internal/api"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/workbuddy"
)

func main() {
	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, resolvedConfigPath, err := config.Load(*configPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	configureRuntimeLogging(cfg)
	credentialKey, credentialKeySource, err := config.LoadOrCreateCredentialEncryptionKey(resolvedConfigPath, cfg)
	if err != nil {
		slog.Error("Failed to load credential encryption key", "error", err)
		os.Exit(1)
	}
	slog.Info("Credential encryption enabled", "key_source", credentialKeySource)

	// DebugEnabled creates per-request files even when verbose diagnostics are
	// disabled, so always apply startup retention in debug mode.
	if cfg.DebugEnabled {
		if err := debug.CleanupAllLogs(); err != nil {
			slog.Warn("清理调试日志失败", "error", err)
		} else {
			slog.Debug("已清空调试日志目录")
		}
	}

	s, err := store.New(store.Options{
		StoreMode:               cfg.StoreMode,
		RedisAddr:               cfg.RedisAddr,
		RedisPassword:           cfg.RedisPassword,
		RedisDB:                 cfg.RedisDB,
		RedisPrefix:             cfg.RedisPrefix,
		CredentialEncryptionKey: credentialKey,
	})
	if err != nil {
		slog.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer s.Close()

	slog.Debug("Store initialized", "mode", "redis", "addr", cfg.RedisAddr, "prefix", cfg.RedisPrefix)

	// 从 Redis 加载已保存的配置（如果存在）
	if savedConfig, err := s.GetSetting(context.Background(), "config"); err == nil && savedConfig != "" {
		if err := json.Unmarshal([]byte(savedConfig), cfg); err != nil {
			slog.Warn("Failed to load config from Redis, using file config", "error", err)
		} else {
			config.ApplyDefaults(cfg)
			configureRuntimeLogging(cfg)
			slog.Debug("Config loaded from Redis")
		}
	}
	if err := grok.ConfigureMediaStorage(cfg); err != nil {
		slog.Error("Failed to initialize media storage", "error", err)
		os.Exit(1)
	}
	slog.Info("Media storage initialized", "directory", cfg.MediaDir, "replicas", cfg.DeploymentReplicas, "shared", cfg.SharedMedia)

	lb := loadbalancer.NewWithCacheTTL(s, time.Duration(cfg.LoadBalancerCacheTTL)*time.Second)

	// Connection tracker: use Redis when available
	var accountTracker loadbalancer.ConnTracker
	if redisClient := s.RedisClient(); redisClient != nil {
		accountTracker = loadbalancer.NewRedisConnTracker(redisClient, s.RedisPrefix())
		lb.SetConnTracker(accountTracker)
		slog.Debug("Connection tracker initialized", "backend", "redis")
	}

	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	if err := apiHandler.EnsureGrokSSOProviderViews(context.Background()); err != nil {
		slog.Error("Failed to reconcile linked Grok SSO provider accounts", "error", err)
	}
	h := handler.NewWithLoadBalancer(cfg, lb)
	defer h.Close()
	grokHandler := grok.NewHandler(cfg, lb)
	if accountTracker != nil {
		grokHandler.SetConnTracker(accountTracker)
	}

	// Token cache: use Redis when available, fall back to memory
	var tokenCache tokencache.Cache
	if redisClient := s.RedisClient(); redisClient != nil {
		tokenCache = tokencache.NewRedisCache(redisClient, s.RedisPrefix(), time.Duration(cfg.CacheTTL)*time.Minute)
		slog.Debug("Token cache initialized", "backend", "redis")
	} else {
		tokenCache = tokencache.NewMemoryCache(time.Duration(cfg.CacheTTL)*time.Minute, 10000)
		slog.Debug("Token cache initialized", "backend", "memory")
	}
	h.SetTokenCache(tokenCache)
	apiHandler.SetTokenCache(tokenCache)

	// Prompt cache: memory-based for now (simulating Anthropic prompt caching)
	promptCache := tokencache.NewMemoryPromptCache(time.Duration(cfg.TokenCacheTTL)*time.Second, 10000)
	h.SetPromptCache(promptCache)
	apiHandler.SetPromptCache(promptCache)
	slog.Debug("Prompt cache initialized", "ttl", cfg.TokenCacheTTL)

	// Session store: use Redis when available, fall back to memory
	if redisClient := s.RedisClient(); redisClient != nil {
		sessionStore := handler.NewRedisSessionStore(redisClient, s.RedisPrefix(), 30*time.Minute)
		h.SetSessionStore(sessionStore)
		slog.Debug("Session store initialized", "backend", "redis")

		auditLogger := audit.NewRedisLogger(redisClient, s.RedisPrefix(), 10000)
		h.SetAuditLogger(auditLogger)
		grokHandler.SetAuditLogger(auditLogger)
		defer auditLogger.Close()
		slog.Debug("Audit logger initialized", "backend", "redis")
	}

	// Provider registry for decoupled client creation
	registry := provider.NewRegistry()
	registry.Register("warp", provider.NewWarpProvider())
	registry.Register("puter", provider.NewPuterProvider())
	registry.Register("workbuddy", provider.NewWorkBuddyProvider())
	h.SetClientFactory(func(acc *store.Account, c *config.Config) handler.UpstreamClient {
		if p := registry.Get(acc.AccountType); p != nil {
			if client, ok := p.NewClient(acc, c).(handler.UpstreamClient); ok {
				// WorkBuddy rotates its refresh token on every renewal; give the
				// client the store so the rotated credential survives the call.
				if wb, ok := client.(interface {
					SetAccountStore(workbuddy.AccountUpdater)
				}); ok {
					wb.SetAccountStore(s)
				}
				return client
			}
		}
		return nil
	})

	// Initialize template renderer
	tmplRenderer, err := template.NewRenderer()
	if err != nil {
		slog.Error("Failed to initialize template renderer", "error", err)
		os.Exit(1)
	}
	slog.Debug("Template renderer initialized")

	// Register routes
	mux := http.NewServeMux()
	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second, cfg.AdaptiveTimeout)
	registerRoutes(mux, cfg, s, h, grokHandler, apiHandler, limiter, accountTracker, tmplRenderer)
	trustedProxy, err := middleware.TrustedProxyMiddleware(cfg.TrustedProxies)
	if err != nil {
		slog.Error("Invalid trusted proxy configuration", "error", err)
		os.Exit(1)
	}

	// Build server
	server := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: middleware.Chain(
			trustedProxy,
			middleware.SecurityHeaders,
			middleware.TraceMiddleware,
			middleware.LoggingMiddleware,
		)(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start background tasks
	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	startTokenRefreshLoop(ctx, cfg, s, lb)
	logWorkBuddyReachability(cfg)

	// Graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		slog.Info("Received signal, starting graceful shutdown", "signal", sig)

		cancelBackground()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Server shutdown error", "error", err)
		}
		close(idleConnsClosed)
	}()

	slog.Info("Server running", "port", cfg.Port)
	slog.Info("Admin UI available", "url", "http://localhost:"+cfg.Port+cfg.AdminPath)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("Server start failed", "error", err)
		os.Exit(1)
	}

	<-idleConnsClosed
	slog.Info("Server shutdown gracefully")
}

// logWorkBuddyReachability reports at startup whether this process can reach the
// WorkBuddy international backend. A blocked egress path breaks both the OAuth
// login and every inference request, so the cause should be visible in the boot
// log instead of surfacing as a per-request 502.
func logWorkBuddyReachability(cfg *config.Config) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		client := workbuddy.NewFromAccount(nil, cfg)
		defer client.Close()
		if err := client.ProbeReachability(ctx); err != nil {
			slog.Warn("WorkBuddy backend is not reachable; the workbuddy channel will fail until egress is fixed",
				"endpoint", workbuddy.DefaultBaseURL, "error", err,
				"hint", "configure HTTP_PROXY/HTTPS_PROXY or the proxy settings in config.json if this host needs one")
			return
		}
		slog.Info("WorkBuddy backend reachable", "endpoint", workbuddy.DefaultBaseURL)
	}()
}

func configureRuntimeLogging(cfg *config.Config) {
	level := slog.LevelInfo
	if cfg != nil && cfg.DebugEnabled {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	logutil.SetVerboseDiagnostics(cfg != nil && cfg.VerboseDiagnosticsEnabled())
}
