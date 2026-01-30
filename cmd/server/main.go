package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"orchids-api/internal/api"
	"orchids-api/internal/auth"
	"orchids-api/internal/clerk"
	"orchids-api/internal/config"
	"orchids-api/internal/constants"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/internal/template"
	"orchids-api/web"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	// 初始化结构化日志
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, resolvedCfgPath, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// 启动时清理所有调试日志
	if cfg.DebugEnabled {
		debug.CleanupAllLogs()
		slog.Info("已清理调试日志目录")
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

	lb := loadbalancer.NewWithCacheTTL(s, time.Duration(cfg.LoadBalancerCacheTTL)*time.Second)
	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg, resolvedCfgPath)
	h := handler.NewWithLoadBalancer(cfg, lb)

	cacheMode := strings.ToLower(cfg.SummaryCacheMode)
	if cacheMode != "off" {
		stats := summarycache.NewStats()
		h.SetSummaryStats(stats)

		var baseCache prompt.SummaryCache
		switch cacheMode {
		case "redis":
			baseCache = summarycache.NewRedisCache(
				cfg.SummaryCacheRedisAddr,
				cfg.SummaryCacheRedisPass,
				cfg.SummaryCacheRedisDB,
				time.Duration(cfg.SummaryCacheTTLSeconds)*time.Second,
				cfg.SummaryCacheRedisPrefix,
			)
		default:
			if cfg.SummaryCacheSize > 0 {
				baseCache = summarycache.NewMemoryCache(cfg.SummaryCacheSize, time.Duration(cfg.SummaryCacheTTLSeconds)*time.Second)
			}
		}

		if baseCache != nil {
			h.SetSummaryCache(summarycache.NewInstrumentedCache(baseCache, stats))
		}
	}
	slog.Info("Summary cache mode", "mode", cacheMode)

	// Initialize template renderer
	tmplRenderer, err := template.NewRenderer()
	if err != nil {
		slog.Error("Failed to initialize template renderer", "error", err)
		os.Exit(1)
	}
	slog.Info("Template renderer initialized")

	mux := http.NewServeMux()

	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second)
	mux.HandleFunc("/v1/messages", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", limiter.Limit(h.HandleCountTokens))

	// Public routes
	mux.HandleFunc("/api/login", apiHandler.HandleLogin)
	mux.HandleFunc("/api/logout", apiHandler.HandleLogout)

	// Admin API with session auth
	mux.HandleFunc("/api/accounts", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleAccounts))
	mux.HandleFunc("/api/accounts/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleAccountByID))
	mux.HandleFunc("/api/keys", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleKeys))
	mux.HandleFunc("/api/keys/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleKeyByID))
	mux.HandleFunc("/api/models", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleModels))
	mux.HandleFunc("/api/models/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleModelByID))
	mux.HandleFunc("/api/export", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleExport))
	mux.HandleFunc("/api/import", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleImport))
	mux.HandleFunc("/api/config", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleConfig))

	// Protected Web UI
	staticHandler := http.StripPrefix(cfg.AdminPath, web.StaticHandler())
	mux.HandleFunc(cfg.AdminPath+"/", func(w http.ResponseWriter, r *http.Request) {
		// Serve login page (static)
		if r.URL.Path == cfg.AdminPath+"/login.html" {
			staticHandler.ServeHTTP(w, r)
			return
		}

		// Serve static assets (CSS, JS)
		if strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/css/") ||
			strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/js/") {
			staticHandler.ServeHTTP(w, r)
			return
		}

		// Authentication check
		cookie, err := r.Cookie("session_token")
		authenticated := err == nil && auth.ValidateSessionToken(cookie.Value)

		if !authenticated {
			adminToken := cfg.AdminToken
			authHeader := r.Header.Get("Authorization")
			authenticated = adminToken != "" && (authHeader == "Bearer "+adminToken || authHeader == adminToken || r.Header.Get("X-Admin-Token") == adminToken)
		}

		if !authenticated {
			http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
			return
		}

		// Render template-based index page
		if r.URL.Path == cfg.AdminPath+"/" || r.URL.Path == cfg.AdminPath {
			err := tmplRenderer.RenderIndex(w, r, cfg)
			if err != nil {
				slog.Error("Failed to render template", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}

		// Fallback to static handler for other files
		staticHandler.ServeHTTP(w, r)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())
	slog.Info("Prometheus metrics enabled", "path", "/metrics")

	if cfg.DebugEnabled {
		mux.HandleFunc("/debug/pprof/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, http.DefaultServeMux.ServeHTTP))
		slog.Info("pprof enabled", "path", "/debug/pprof/")
	}

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Create context for background goroutines
	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	if cfg.AutoRefreshToken {
		interval := time.Duration(cfg.TokenRefreshInterval) * time.Minute
		if interval <= 0 {
			interval = 30 * time.Minute
		}
		slog.Info("Auto refresh token enabled", "interval", interval.String())

		refreshAccounts := func() {
			accounts, err := s.GetEnabledAccounts(context.Background())
			if err != nil {
				slog.Error("Auto refresh token: list accounts failed", "error", err)
				return
			}
			for _, acc := range accounts {
				if strings.TrimSpace(acc.ClientCookie) == "" {
					continue
				}
				info, err := clerk.FetchAccountInfo(acc.ClientCookie)
				if err != nil {
					slog.Warn("Auto refresh token failed", "account", acc.Name, "error", err)
					continue
				}
				if info.SessionID != "" {
					acc.SessionID = info.SessionID
				}
				if info.ClientUat != "" {
					acc.ClientUat = info.ClientUat
				}
				if info.ProjectID != "" {
					acc.ProjectID = info.ProjectID
				}
				if info.UserID != "" {
					acc.UserID = info.UserID
				}
				if info.Email != "" {
					acc.Email = info.Email
				}
				if info.JWT != "" {
					acc.Token = info.JWT
				}
				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "error", err)
					continue
				}
			}
		}

		go func() {
			refreshAccounts()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					refreshAccounts()
				}
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(constants.SessionCleanupPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				auth.CleanupExpiredSessions()
			}
		}
	}()

	// 优雅关闭处理
	idleConnsClosed := make(chan struct{})
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		slog.Info("Received signal, starting graceful shutdown", "signal", sig)

		// Stop background goroutines first
		cancelBackground()

		// Give existing requests time to complete
		shutdownCtx, cancel := context.WithTimeout(context.Background(), constants.ShutdownTimeout)
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
