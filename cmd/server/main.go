package main

import (
	"context"
	"encoding/json"
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
	"orchids-api/internal/debug"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/warp"
	"orchids-api/web"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, resolvedCfgPath, err := config.Load(*configPath)
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
			// 重新应用默认值，防止 Redis 中缺少新增字段导致零值覆盖
			config.ApplyDefaults(cfg)
			// Enforce lower refresh interval if it's too high (legacy default was 30)
			if cfg.TokenRefreshInterval > 5 {
				slog.Info("Enforcing lower token refresh interval", "old", cfg.TokenRefreshInterval, "new", 1)
				cfg.TokenRefreshInterval = 1
			}
			// Enforce higher request timeout (legacy default was 120)
			if cfg.RequestTimeout < 300 {
				slog.Info("Enforcing higher request timeout", "old", cfg.RequestTimeout, "new", 600)
				cfg.RequestTimeout = 600
			}
		}
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Duration(cfg.LoadBalancerCacheTTL)*time.Second)
	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg, resolvedCfgPath)
	h := handler.NewWithLoadBalancer(cfg, lb)
	grokHandler := grok.NewHandler(cfg, lb)

	tokenCache := tokencache.NewMemoryCache(time.Duration(cfg.CacheTTL)*time.Minute, 10000)
	h.SetTokenCache(tokenCache)
	apiHandler.SetTokenCache(tokenCache)

	// Summary cache disabled (removed).

	// Initialize template renderer
	tmplRenderer, err := template.NewRenderer()
	if err != nil {
		slog.Error("Failed to initialize template renderer", "error", err)
		os.Exit(1)
	}
	slog.Info("Template renderer initialized")

	mux := http.NewServeMux()

	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second, cfg.AdaptiveTimeout)
	mux.HandleFunc("/orchids/v1/messages", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/orchids/v1/messages/count_tokens", limiter.Limit(h.HandleCountTokens))
	mux.HandleFunc("/warp/v1/messages", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/warp/v1/messages/count_tokens", limiter.Limit(h.HandleCountTokens))
	// Public Model Routes (Orchids & Warp separate channels)
	mux.HandleFunc("/orchids/v1/models", h.HandleModels)
	mux.HandleFunc("/orchids/v1/models/", h.HandleModelByID)
	mux.HandleFunc("/warp/v1/models", h.HandleModels)
	mux.HandleFunc("/warp/v1/models/", h.HandleModelByID)
	mux.HandleFunc("/grok/v1/models", h.HandleModels)
	mux.HandleFunc("/grok/v1/models/", h.HandleModelByID)
	// Unified Model Routes (All channels)
	mux.HandleFunc("/v1/models", h.HandleModels)
	mux.HandleFunc("/v1/models/", h.HandleModelByID)

	// OpenAI Compatibility - Channel Specific
	mux.HandleFunc("/orchids/v1/chat/completions", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/warp/v1/chat/completions", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/grok/v1/chat/completions", limiter.Limit(grokHandler.HandleChatCompletions))
	mux.HandleFunc("/grok/v1/images/generations", limiter.Limit(grokHandler.HandleImagesGenerations))
	mux.HandleFunc("/grok/v1/images/edits", limiter.Limit(grokHandler.HandleImagesEdits))
	mux.HandleFunc("/grok/v1/files/", grokHandler.HandleFiles)
	// grok2api-compatible aliases
	mux.HandleFunc("/v1/chat/completions", limiter.Limit(grokHandler.HandleChatCompletions))
	mux.HandleFunc("/v1/images/generations", limiter.Limit(grokHandler.HandleImagesGenerations))
	mux.HandleFunc("/v1/images/edits", limiter.Limit(grokHandler.HandleImagesEdits))
	mux.HandleFunc("/v1/files/", grokHandler.HandleFiles)

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
	mux.HandleFunc("/api/v1/admin/config", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleConfig))
	mux.HandleFunc("/api/config/cache/stats", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleCacheStats))
	mux.HandleFunc("/api/config/cache/clear", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleCacheClear))
	mux.HandleFunc("/api/v1/admin/voice/token", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminVoiceToken))
	mux.HandleFunc("/api/v1/admin/imagine/start", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminImagineStart))
	mux.HandleFunc("/api/v1/admin/imagine/stop", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminImagineStop))
	mux.HandleFunc("/api/v1/admin/imagine/sse", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminImagineSSE))
	mux.HandleFunc("/api/v1/admin/imagine/ws", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminImagineWS))
	mux.HandleFunc("/api/v1/admin/cache", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCache))
	mux.HandleFunc("/api/v1/admin/cache/list", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheList))
	mux.HandleFunc("/api/v1/admin/cache/clear", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheClear))
	mux.HandleFunc("/api/v1/admin/cache/item/delete", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheItemDelete))
	mux.HandleFunc("/api/v1/admin/cache/online/clear", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineClear))
	mux.HandleFunc("/api/v1/admin/cache/online/clear/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineClearAsync))
	mux.HandleFunc("/api/v1/admin/cache/online/load/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineLoadAsync))
	mux.HandleFunc("/api/v1/admin/verify", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminVerify))
	mux.HandleFunc("/api/v1/admin/storage", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminStorage))
	mux.HandleFunc("/api/v1/admin/tokens", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokens))
	mux.HandleFunc("/api/v1/admin/tokens/refresh", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokensRefresh))
	mux.HandleFunc("/api/v1/admin/tokens/refresh/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokensRefreshAsync))
	mux.HandleFunc("/api/v1/admin/tokens/nsfw/enable", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminNSFWEnable))
	mux.HandleFunc("/api/v1/admin/tokens/nsfw/enable/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminNSFWEnableAsync))
	mux.HandleFunc("/api/v1/admin/batch/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminBatchTask))
	// grok2api admin-compatible aliases
	mux.HandleFunc("/v1/admin/verify", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminVerify))
	mux.HandleFunc("/v1/admin/config", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, apiHandler.HandleConfig))
	mux.HandleFunc("/v1/admin/storage", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminStorage))
	mux.HandleFunc("/v1/admin/tokens", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokens))
	mux.HandleFunc("/v1/admin/tokens/refresh", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokensRefresh))
	mux.HandleFunc("/v1/admin/tokens/refresh/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminTokensRefreshAsync))
	mux.HandleFunc("/v1/admin/tokens/nsfw/enable", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminNSFWEnable))
	mux.HandleFunc("/v1/admin/tokens/nsfw/enable/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminNSFWEnableAsync))
	mux.HandleFunc("/v1/admin/batch/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminBatchTask))
	mux.HandleFunc("/v1/admin/cache", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCache))
	mux.HandleFunc("/v1/admin/cache/list", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheList))
	mux.HandleFunc("/v1/admin/cache/clear", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheClear))
	mux.HandleFunc("/v1/admin/cache/item/delete", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheItemDelete))
	mux.HandleFunc("/v1/admin/cache/online/clear", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineClear))
	mux.HandleFunc("/v1/admin/cache/online/clear/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineClearAsync))
	mux.HandleFunc("/v1/admin/cache/online/load/async", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, grokHandler.HandleAdminCacheOnlineLoadAsync))
	// grok2api public-compatible endpoints
	// Read public auth config per request so Admin config updates take effect immediately.
	publicAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			middleware.PublicKeyAuth(cfg.PublicAPIKey(), cfg.PublicAPIEnabled(), next)(w, r)
		}
	}
	publicImagineStreamAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			middleware.PublicImagineStreamAuth(cfg.PublicAPIKey(), cfg.PublicAPIEnabled(), next)(w, r)
		}
	}

	mux.HandleFunc("/api/v1/public/verify", publicAuth(grokHandler.HandlePublicVerify))
	mux.HandleFunc("/api/v1/public/voice/token", publicAuth(grokHandler.HandleAdminVoiceToken))
	mux.HandleFunc("/api/v1/public/imagine/config", grokHandler.HandlePublicImagineConfig)
	mux.HandleFunc("/api/v1/public/imagine/start", publicAuth(grokHandler.HandleAdminImagineStart))
	mux.HandleFunc("/api/v1/public/imagine/stop", publicAuth(grokHandler.HandleAdminImagineStop))
	mux.HandleFunc("/api/v1/public/imagine/sse", publicImagineStreamAuth(grokHandler.HandleAdminImagineSSE))
	mux.HandleFunc("/api/v1/public/imagine/ws", publicImagineStreamAuth(grokHandler.HandleAdminImagineWS))
	mux.HandleFunc("/api/v1/public/video/start", publicAuth(grokHandler.HandlePublicVideoStart))
	mux.HandleFunc("/api/v1/public/video/stop", publicAuth(grokHandler.HandlePublicVideoStop))
	mux.HandleFunc("/api/v1/public/video/sse", grokHandler.HandlePublicVideoSSE)
	mux.HandleFunc("/v1/public/verify", publicAuth(grokHandler.HandlePublicVerify))
	mux.HandleFunc("/v1/public/voice/token", publicAuth(grokHandler.HandleAdminVoiceToken))
	mux.HandleFunc("/v1/public/imagine/config", grokHandler.HandlePublicImagineConfig)
	mux.HandleFunc("/v1/public/imagine/start", publicAuth(grokHandler.HandleAdminImagineStart))
	mux.HandleFunc("/v1/public/imagine/stop", publicAuth(grokHandler.HandleAdminImagineStop))
	mux.HandleFunc("/v1/public/imagine/sse", publicImagineStreamAuth(grokHandler.HandleAdminImagineSSE))
	mux.HandleFunc("/v1/public/imagine/ws", publicImagineStreamAuth(grokHandler.HandleAdminImagineWS))
	mux.HandleFunc("/v1/public/video/start", publicAuth(grokHandler.HandlePublicVideoStart))
	mux.HandleFunc("/v1/public/video/stop", publicAuth(grokHandler.HandlePublicVideoStop))
	mux.HandleFunc("/v1/public/video/sse", grokHandler.HandlePublicVideoSSE)

	// Public static assets/pages
	staticRootHandler := web.StaticHandler()
	mux.Handle("/static/", http.StripPrefix("/static/", staticRootHandler))

	serveStaticPath := func(w http.ResponseWriter, r *http.Request, path string) {
		target := "/" + strings.TrimPrefix(strings.TrimSpace(path), "/")
		if target == "/" || strings.Contains(target, "..") {
			http.NotFound(w, r)
			return
		}
		rr := r.Clone(r.Context())
		rr.URL.Path = target
		staticRootHandler.ServeHTTP(w, rr)
	}

	servePublicPage := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !cfg.PublicAPIEnabled() {
				http.NotFound(w, r)
				return
			}
			serveStaticPath(w, r, path)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if cfg.PublicAPIEnabled() {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
	})
	mux.HandleFunc("/login", servePublicPage("public/pages/login.html"))
	mux.HandleFunc("/imagine", servePublicPage("public/pages/imagine.html"))
	mux.HandleFunc("/voice", servePublicPage("public/pages/voice.html"))
	mux.HandleFunc("/video", servePublicPage("public/pages/video.html"))

	// grok2api public page-compatible aliases
	redirectPublicRoot := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !cfg.PublicAPIEnabled() {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
	mux.HandleFunc("/v1/public", redirectPublicRoot)
	mux.HandleFunc("/v1/public/", redirectPublicRoot)
	mux.HandleFunc("/api/v1/public", redirectPublicRoot)
	mux.HandleFunc("/api/v1/public/", redirectPublicRoot)
	mux.HandleFunc("/v1/public/login", servePublicPage("public/pages/login.html"))
	mux.HandleFunc("/v1/public/imagine", servePublicPage("public/pages/imagine.html"))
	mux.HandleFunc("/v1/public/voice", servePublicPage("public/pages/voice.html"))
	mux.HandleFunc("/v1/public/video", servePublicPage("public/pages/video.html"))
	mux.HandleFunc("/api/v1/public/login", servePublicPage("public/pages/login.html"))
	mux.HandleFunc("/api/v1/public/imagine", servePublicPage("public/pages/imagine.html"))
	mux.HandleFunc("/api/v1/public/voice", servePublicPage("public/pages/voice.html"))
	mux.HandleFunc("/api/v1/public/video", servePublicPage("public/pages/video.html"))

	// Protected Web UI
	staticHandler := http.StripPrefix(cfg.AdminPath, staticRootHandler)
	isAdminAuthenticated := func(r *http.Request) bool {
		cookie, err := r.Cookie("session_token")
		authenticated := err == nil && auth.ValidateSessionToken(cookie.Value)
		if authenticated {
			return true
		}
		adminToken := cfg.AdminToken
		authHeader := r.Header.Get("Authorization")
		return adminToken != "" && (authHeader == "Bearer "+adminToken || authHeader == adminToken || r.Header.Get("X-Admin-Token") == adminToken)
	}
	renderAdminIndex := func(w http.ResponseWriter, r *http.Request) {
		err := tmplRenderer.RenderIndex(w, r, cfg, s)
		if err != nil {
			slog.Error("Failed to render template", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}

	// grok2api admin page-compatible aliases
	mux.HandleFunc(cfg.AdminPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, cfg.AdminPath+"/", http.StatusFound)
	})
	mux.HandleFunc(cfg.AdminPath+"/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rr := r.Clone(r.Context())
		rr.URL.Path = cfg.AdminPath + "/login.html"
		staticHandler.ServeHTTP(w, rr)
	})
	adminAliasPage := func(path string) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !isAdminAuthenticated(r) {
				http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
				return
			}
			renderAdminIndex(w, r)
		})
	}
	adminAliasPage(cfg.AdminPath + "/config")
	adminAliasPage(cfg.AdminPath + "/cache")
	adminAliasPage(cfg.AdminPath + "/token")

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

		if !isAdminAuthenticated(r) {
			http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
			return
		}

		// Render template-based index page
		if r.URL.Path == cfg.AdminPath+"/" || r.URL.Path == cfg.AdminPath {
			renderAdminIndex(w, r)
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
		Addr: ":" + cfg.Port,
		Handler: middleware.Chain(
			middleware.TraceMiddleware,
			middleware.LoggingMiddleware,
		)(mux),
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
				if strings.EqualFold(acc.AccountType, "warp") {
					if !acc.QuotaResetAt.IsZero() && time.Now().Before(acc.QuotaResetAt) {
						continue
					}
					if strings.TrimSpace(acc.RefreshToken) == "" && strings.TrimSpace(acc.ClientCookie) == "" {
						continue
					}
					warpClient := warp.NewFromAccount(acc, cfg)
					jwt, err := warpClient.RefreshAccount(context.Background())
					if err != nil {
						retryAfter := warp.RetryAfter(err)
						httpStatus := warp.HTTPStatusCode(err)
						// 标记 401/403 账号状态
						if httpStatus == 401 || httpStatus == 403 {
							lb.MarkAccountStatus(context.Background(), acc, fmt.Sprintf("%d", httpStatus))
						} else if retryAfter > 0 {
							// 429 等限流错误，只记录 QuotaResetAt
							acc.QuotaResetAt = time.Now().Add(retryAfter)
							if updateErr := s.UpdateAccount(context.Background(), acc); updateErr != nil {
								slog.Warn("Auto refresh token: record warp retry-after failed", "account", acc.Name, "type", "warp", "error", updateErr)
							}
						}
						slog.Warn("Auto refresh token failed", "account", acc.Name, "type", "warp", "http_status", httpStatus, "error", err)
						continue
					}
					if jwt != "" {
						acc.Token = jwt
					}
					warpClient.SyncAccountState()

					// Sync Warp usage quota via GraphQL
					limitCtx, limitCancel := context.WithTimeout(context.Background(), 15*time.Second)
					limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
					limitCancel()
					if limitErr != nil {
						slog.Warn("Warp usage sync failed", "account", acc.Name, "error", limitErr)
					} else if limitInfo != nil {
						if limitInfo.IsUnlimited {
							acc.Subscription = "unlimited"
						} else {
							acc.Subscription = "free"
						}
						totalLimit := float64(limitInfo.RequestLimit)
						for _, bg := range bonuses {
							totalLimit += float64(bg.RequestCreditsRemaining)
						}
						usedRequests := float64(limitInfo.RequestsUsedSinceLastRefresh)
						acc.UsageLimit = totalLimit
						// Warp: UsageCurrent 语义为“已使用请求数”
						acc.UsageCurrent = usedRequests
						if limitInfo.NextRefreshTime != "" {
							if t, err := time.Parse(time.RFC3339, limitInfo.NextRefreshTime); err == nil {
								acc.QuotaResetAt = t
							}
						}
						slog.Debug("Warp usage synced", "account", acc.Name, "limit", acc.UsageLimit, "used", acc.UsageCurrent, "subscription", acc.Subscription)
					}

					if err := s.UpdateAccount(context.Background(), acc); err != nil {
						slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "warp", "error", err)
					}
					continue
				}
				// Grok accounts store SSO tokens in ClientCookie and are not Clerk-backed.
				// Skip auto-refresh to avoid false 401s and cooldown.
				if strings.EqualFold(acc.AccountType, "grok") {
					continue
				}
				if strings.TrimSpace(acc.ClientCookie) == "" {
					continue
				}
				info, err := clerk.FetchAccountInfo(acc.ClientCookie)
				if err != nil {
					errLower := strings.ToLower(err.Error())
					switch {
					case strings.Contains(errLower, "status code 401") || strings.Contains(errLower, "unauthorized"):
						lb.MarkAccountStatus(context.Background(), acc, "401")
					case strings.Contains(errLower, "status code 403") || strings.Contains(errLower, "forbidden"):
						lb.MarkAccountStatus(context.Background(), acc, "403")
					case strings.Contains(errLower, "no active sessions"):
						lb.MarkAccountStatus(context.Background(), acc, "401")
					}
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
				if info.ClientCookie != "" {
					acc.ClientCookie = info.ClientCookie
				}

				// Sync Orchids credits via RSC Server Action
				if info.JWT != "" {
					creditsCtx, creditsCancel := context.WithTimeout(context.Background(), 15*time.Second)
					uid := info.UserID
					if strings.TrimSpace(uid) == "" {
						uid = acc.UserID
					}
					creditsInfo, creditsErr := orchids.FetchCredits(creditsCtx, info.JWT, uid)
					creditsCancel()
					if creditsErr != nil {
						slog.Warn("Orchids credits sync failed", "account", acc.Name, "error", creditsErr)
					} else if creditsInfo != nil {
						acc.Subscription = strings.ToLower(creditsInfo.Plan)
						acc.UsageCurrent = creditsInfo.Credits
						acc.UsageLimit = orchids.PlanCreditLimit(creditsInfo.Plan)
						slog.Debug("Orchids credits synced", "account", acc.Name, "credits", acc.UsageCurrent, "limit", acc.UsageLimit, "plan", acc.Subscription)
					}
				}

				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "error", err)
					continue
				}
			}
		}

		go func() {
			defer func() {
				if err := recover(); err != nil {
					slog.Error("Panic in token refresh loop", "error", err)
				}
			}()
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
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in auth cleanup loop", "error", err)
			}
		}()
		ticker := time.NewTicker(time.Hour)
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

	// 上游模型同步
	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in upstream model sync loop", "error", err)
			}
		}()

		syncModels := func() {
			accounts, err := s.GetEnabledAccounts(context.Background())
			if err != nil {
				slog.Warn("上游模型同步: 获取账号失败", "error", err)
				return
			}
			// 找到第一个可用的 Orchids 账号来获取上游模型
			var client *orchids.Client
			hasOrchidsAccount := false
			for _, acc := range accounts {
				if strings.EqualFold(acc.AccountType, "warp") {
					continue
				}
				hasOrchidsAccount = true
				client = orchids.NewFromAccount(acc, cfg)
				break
			}
			if client == nil {
				if !hasOrchidsAccount {
					slog.Debug("上游模型同步: 无 Orchids 账号，跳过")
					return
				}
				// 兜底：存在 Orchids 账号但构造失败时仍尝试默认配置
				client = orchids.New(cfg)
			}

			fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			upstreamModels, err := client.FetchUpstreamModels(fetchCtx)
			if err != nil {
				slog.Warn("上游模型同步: 获取失败", "error", err)
				return
			}
			if len(upstreamModels) == 0 {
				slog.Debug("上游模型同步: 无模型返回")
				return
			}

			added := 0
			for _, um := range upstreamModels {
				modelID := strings.TrimSpace(um.ID)
				if modelID == "" {
					continue
				}
				// 检查本地是否已存在
				if _, err := s.GetModelByModelID(context.Background(), modelID); err == nil {
					continue
				}
				// 新模型，添加到 store
				channel := "Orchids"
				if strings.TrimSpace(um.OwnedBy) != "" {
					channel = um.OwnedBy
				}
				newModel := &store.Model{
					Channel: channel,
					ModelID: modelID,
					Name:    modelID,
					Status:  store.ModelStatusAvailable,
				}
				if err := s.CreateModel(context.Background(), newModel); err != nil {
					slog.Warn("上游模型同步: 创建模型失败", "model_id", modelID, "error", err)
					continue
				}
				added++
				slog.Info("上游模型同步: 新增模型", "model_id", modelID, "channel", channel)
			}
			if added > 0 {
				slog.Info("上游模型同步完成", "total_upstream", len(upstreamModels), "added", added)
			} else {
				slog.Debug("上游模型同步完成，无新增", "total_upstream", len(upstreamModels))
			}
		}

		syncWarpModels := func() {
			accounts, err := s.GetEnabledAccounts(context.Background())
			if err != nil {
				slog.Warn("Warp 模型同步: 获取账号失败", "error", err)
				return
			}
			// 找到第一个可用的 Warp 账号
			var warpAcc *store.Account
			for _, acc := range accounts {
				if strings.EqualFold(acc.AccountType, "warp") && strings.TrimSpace(acc.Token) != "" {
					warpAcc = acc
					break
				}
			}
			if warpAcc == nil {
				slog.Debug("Warp 模型同步: 无可用 Warp 账号")
				return
			}

			warpClient := warp.NewFromAccount(warpAcc, cfg)
			fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			choices, err := warpClient.GetFeatureModelChoices(fetchCtx)
			if err != nil {
				slog.Warn("Warp 模型同步: 获取失败", "error", err)
				return
			}

			// Collect all unique models from all categories
			seen := make(map[string]bool)
			added := 0
			categories := []*warp.FeatureModelCategory{choices.AgentMode, choices.Planning, choices.Coding, choices.CliAgent}
			for _, cat := range categories {
				if cat == nil {
					continue
				}
				for _, choice := range cat.Choices {
					modelID := strings.TrimSpace(choice.ID)
					if modelID == "" || seen[modelID] {
						continue
					}
					seen[modelID] = true
					// Check if model already exists
					if _, err := s.GetModelByModelID(context.Background(), modelID); err == nil {
						continue
					}
					displayName := choice.DisplayName
					if displayName == "" {
						displayName = modelID
					}
					newModel := &store.Model{
						Channel: "Warp",
						ModelID: modelID,
						Name:    displayName + " (Warp)",
						Status:  store.ModelStatusAvailable,
					}
					if err := s.CreateModel(context.Background(), newModel); err != nil {
						slog.Warn("Warp 模型同步: 创建模型失败", "model_id", modelID, "error", err)
						continue
					}
					added++
					slog.Info("Warp 模型同步: 新增模型", "model_id", modelID, "name", displayName)
				}
			}
			if added > 0 {
				slog.Info("Warp 模型同步完成", "added", added)
			} else {
				slog.Debug("Warp 模型同步完成，无新增")
			}
		}

		// 启动时延迟 10 秒执行，等待 token 刷新完成
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		syncModels()
		syncWarpModels()

		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncModels()
				syncWarpModels()
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
