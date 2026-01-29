package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/web"
)

func main() {
	configPath := flag.String("config", "", "Path to config.json/config.yaml")
	flag.Parse()

	cfg, resolvedCfgPath, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// 启动时清理所有调试日志
	if cfg.DebugEnabled {
		debug.CleanupAllLogs()
		log.Println("已清理调试日志目录")
	}

	storeMode := strings.ToLower(strings.TrimSpace(cfg.StoreMode))
	dbPath := ""
	if storeMode != "redis" {
		dataDir := filepath.Join(".", "data")
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			log.Fatalf("Failed to create data dir: %v", err)
		}
		dbPath = filepath.Join(dataDir, "orchids.db")
	}

	s, err := store.New(dbPath, store.Options{
		StoreMode:     cfg.StoreMode,
		RedisAddr:     cfg.RedisAddr,
		RedisPassword: cfg.RedisPassword,
		RedisDB:       cfg.RedisDB,
		RedisPrefix:   cfg.RedisPrefix,
	})
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer s.Close()

	if storeMode == "redis" {
		log.Printf("Store mode: redis (addr=%s, prefix=%s)", cfg.RedisAddr, cfg.RedisPrefix)
	} else {
		log.Printf("Store mode: sqlite (db=%s)", dbPath)
	}

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
	log.Printf("Summary cache mode: %s", cacheMode)

	mux := http.NewServeMux()

	limiter := middleware.NewConcurrencyLimiter(cfg.ConcurrencyLimit, time.Duration(cfg.ConcurrencyTimeout)*time.Second)
	mux.HandleFunc("/v1/messages", limiter.Limit(h.HandleMessages))

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
	adminGroup := http.StripPrefix(cfg.AdminPath, web.StaticHandler())
	mux.HandleFunc(cfg.AdminPath+"/", func(w http.ResponseWriter, r *http.Request) {
		// Allow login.html to be served without auth
		if r.URL.Path == cfg.AdminPath+"/login.html" {
			adminGroup.ServeHTTP(w, r)
			return
		}

		// Check session
		token := fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.AdminPass)))
		cookie, err := r.Cookie("session_token")

		// Also allow AdminToken for direct access if provided via header
		adminToken := cfg.AdminToken
		authHeader := r.Header.Get("Authorization")
		authenticated := (err == nil && cookie.Value == token) ||
			(adminToken != "" && (authHeader == "Bearer "+adminToken || authHeader == adminToken || r.Header.Get("X-Admin-Token") == adminToken))

		if !authenticated {
			// Redirect browser requests to login.html
			http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
			return
		}

		// If authenticated and path is just the admin root, it will serve index.html via FileServer
		// We can also explicitly serve other static assets (if any)
		adminGroup.ServeHTTP(w, r)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	if cfg.DebugEnabled {
		mux.HandleFunc("/debug/pprof/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, http.DefaultServeMux.ServeHTTP))
		log.Println("pprof 性能监控已启用: /debug/pprof/")
	}

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Server running on port %s", cfg.Port)
	log.Printf("Admin UI: http://localhost:%s%s", cfg.Port, cfg.AdminPath)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
