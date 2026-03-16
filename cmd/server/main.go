package main

import (
	"context"
	"flag"
	"fmt"
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
	"orchids-api/internal/middleware"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/tokencache"
)

func main() {
	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, _, err := config.Load(*configPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// 根据配置初始化日志级别
	var level slog.Level = slog.LevelInfo
	if cfg.DebugEnabled {
		level = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// 启动时清空所有调试日志
	if cfg.DebugEnabled {
		if err := debug.CleanupAllLogs(); err != nil {
			slog.Warn("清理调试日志失败", "error", err)
		} else {
			slog.Info("已清空调试日志目录")
		}
	}

	s, err := store.New(store.Options{
		StoreMode:     cfg.StoreMode,
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		RedisDB:       cfg.RedisDB,
		RedisPrefix:   cfg.RedisPrefix,
	})
	if err != nil {
		slog.Error("Failed to initialize database", "error", err)
		os.Exit(1)
	}
	defer s.Close()

	slog.Info("Store initialized", "mode", "redis", "addr", cfg.RedisAddr, "prefix", cfg.RedisPrefix)

	// 从 Redis 加载已保存的配置（如果存在）
	if savedConfig, err := s.GetSetting(context.Background(), "config"); err == nil && savedConfig != "" {
		if err := json.Unmarshal([]byte(savedConfig), cfg); err != nil {
			slog.Warn("Failed to load config from Redis, using file config", "error", err)
		} else {
			slog.Info("Config loaded from Redis")
			config.ApplyDefaults(cfg)
		}
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Duration(cfg.LoadBalancerCacheTTL)*time.Second)

	// Connection tracker: use Redis when available
	if redisClient := s.RedisClient(); redisClient != nil {
		lb.SetConnTracker(loadbalancer.NewRedisConnTracker(redisClient, s.RedisPrefix()))
		slog.Info("Connection tracker initialized", "backend", "redis")
	}

	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	h := handler.NewWithLoadBalancer(cfg, lb)
	defer h.Close()
	grokHandler := grok.NewHandler(cfg, lb)

	// Token cache: use Redis when available, fall back to memory
	var tokenCache tokencache.Cache
	if redisClient := s.RedisClient(); redisClient != nil {
		tokenCache = tokencache.NewRedisCache(redisClient, s.RedisPrefix(), time.Duration(cfg.CacheTTL)*time.Minute)
		slog.Info("Token cache initialized", "backend", "redis")
	} else {
		tokenCache = tokencache.NewMemoryCache(time.Duration(cfg.CacheTTL)*time.Minute, 10000)
		slog.Info("Token cache initialized", "backend", "memory")
	}
	h.SetTokenCache(tokenCache)
	apiHandler.SetTokenCache(tokenCache)

	// Prompt cache: memory-based for now (simulating Anthropic prompt caching)
	promptCache := tokencache.NewMemoryPromptCache(time.Duration(cfg.TokenCacheTTL)*time.Second, 10000)
	h.SetPromptCache(promptCache)
	apiHandler.SetPromptCache(promptCache)
	slog.Info("Prompt cache initialized", "ttl", cfg.TokenCacheTTL)

	// Session store: use Redis when available, fall back to memory
	if redisClient := s.RedisClient(); redisClient != nil {
		sessionStore := handler.NewRedisSessionStore(redisClient, s.RedisPrefix(), 30*time.Minute)
		h.SetSessionStore(sessionStore)
		slog.Info("Session store initialized", "backend", "redis")

		dedupStore := handler.NewRedisDedupStore(redisClient, s.RedisPrefix(), 2*time.Second)
		h.SetDedupStore(dedupStore)
		slog.Info("Dedup store initialized", "backend", "redis")

		auditLogger := audit.NewRedisLogger(redisClient, s.RedisPrefix(), 10000)
		h.SetAuditLogger(auditLogger)
		defer auditLogger.Close()
		slog.Info("Audit logger initialized", "backend", "redis")
	}

	// Provider registry for decoupled client creation
	registry := provider.NewRegistry()
	registry.Register("orchids", provider.NewOrchidsProvider())
	registry.Register("warp", provider.NewWarpProvider())
	h.SetClientFactory(func(acc *store.Account, c *config.Config) handler.UpstreamClient {
		if p := registry.Get(acc.AccountType); p != nil {
			if client, ok := p.NewClient(acc, c).(handler.UpstreamClient); ok {
				return client
			}
		}
		// Fallback to orchids
		if p := registry.Get("orchids"); p != nil {
			if client, ok := p.NewClient(acc, c).(handler.UpstreamClient); ok {
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
	slog.Info("Template renderer initialized")

	// Register routes
	mux := http.NewServeMux()
	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second, cfg.AdaptiveTimeout)
	registerRoutes(mux, cfg, s, h, grokHandler, apiHandler, limiter, tmplRenderer)

	// Build server
	server := &http.Server{
		Addr: ":" + cfg.Port,
		Handler: middleware.Chain(
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
	startAuthCleanupLoop(ctx)
	startModelSyncLoop(ctx, cfg, s)

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
	slog.Info("Admin UI available", "url", fmt.Sprintf("http://localhost:%s%s", cfg.Port, cfg.AdminPath))

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("Server start failed", "error", err)
		os.Exit(1)
	}

	<-idleConnsClosed
	slog.Info("Server shutdown gracefully")
}
