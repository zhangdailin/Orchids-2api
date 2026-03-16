package main

import (
	"log/slog"
	"net/http"
	"strings"

	"orchids-api/internal/api"
	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/web"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registerWithPrefixes registers the same handler under multiple prefix+path combinations.
func registerWithPrefixes(mux *http.ServeMux, prefixes []string, path string, h http.HandlerFunc) {
	for _, p := range prefixes {
		mux.HandleFunc(p+path, h)
	}
}

func registerRoutes(
	mux *http.ServeMux,
	cfg *config.Config,
	s *store.Store,
	h *handler.Handler,
	grokHandler *grok.Handler,
	apiHandler *api.API,
	limiter *middleware.ConcurrencyLimiter,
	tmplRenderer *template.Renderer,
) {
	// --- Channel-specific message routes ---
	mux.HandleFunc("/orchids/v1/messages", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/orchids/v1/messages/count_tokens", limiter.Limit(h.HandleCountTokens))
	mux.HandleFunc("/warp/v1/messages", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/warp/v1/messages/count_tokens", limiter.Limit(h.HandleCountTokens))

	// --- Model routes (4 channel prefixes → same handlers) ---
	modelPrefixes := []string{"/orchids/v1", "/warp/v1", "/grok/v1", "/v1"}
	registerWithPrefixes(mux, modelPrefixes, "/models", h.HandleModels)
	registerWithPrefixes(mux, modelPrefixes, "/models/", h.HandleModelByID)

	// --- OpenAI-compatible chat/image routes (channel-specific + unified) ---
	mux.HandleFunc("/orchids/v1/chat/completions", limiter.Limit(h.HandleMessages))
	mux.HandleFunc("/warp/v1/chat/completions", limiter.Limit(h.HandleMessages))

	grokPrefixes := []string{"/grok/v1", "/v1"}
	registerWithPrefixes(mux, grokPrefixes, "/chat/completions", limiter.Limit(grokHandler.HandleChatCompletions))
	registerWithPrefixes(mux, grokPrefixes, "/images/generations", limiter.Limit(grokHandler.HandleImagesGenerations))
	registerWithPrefixes(mux, grokPrefixes, "/images/edits", limiter.Limit(grokHandler.HandleImagesEdits))
	registerWithPrefixes(mux, grokPrefixes, "/files/", grokHandler.HandleFiles)

	// --- Public auth/login (no prefix duplication) ---
	mux.HandleFunc("/api/login", apiHandler.HandleLogin)
	mux.HandleFunc("/api/logout", apiHandler.HandleLogout)

	// --- Admin API routes (session auth, dual prefix) ---
	sessionAuth := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, h)
	}

	// Admin routes under /api/* only (no dual prefix)
	mux.HandleFunc("/api/accounts", sessionAuth(apiHandler.HandleAccounts))
	mux.HandleFunc("/api/accounts/", sessionAuth(apiHandler.HandleAccountByID))
	mux.HandleFunc("/api/keys", sessionAuth(apiHandler.HandleKeys))
	mux.HandleFunc("/api/keys/", sessionAuth(apiHandler.HandleKeyByID))
	mux.HandleFunc("/api/models", sessionAuth(apiHandler.HandleModels))
	mux.HandleFunc("/api/models/", sessionAuth(apiHandler.HandleModelByID))
	mux.HandleFunc("/api/export", sessionAuth(apiHandler.HandleExport))
	mux.HandleFunc("/api/import", sessionAuth(apiHandler.HandleImport))
	mux.HandleFunc("/api/config", sessionAuth(apiHandler.HandleConfig))
	mux.HandleFunc("/api/config/cache/clear", sessionAuth(apiHandler.HandleCacheClear))
	mux.HandleFunc("/api/token-cache/stats", sessionAuth(apiHandler.HandleTokenCacheStats))
	mux.HandleFunc("/api/token-cache/clear", sessionAuth(apiHandler.HandleTokenCacheClear))

	// Admin routes with dual prefix: /api/v1/admin/* and /v1/admin/*
	adminPrefixes := []string{"/api/v1/admin", "/v1/admin"}
	adminRoutes := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/config", apiHandler.HandleConfig},
		{"/verify", grokHandler.HandleAdminVerify},
		{"/storage", grokHandler.HandleAdminStorage},
		{"/tokens", grokHandler.HandleAdminTokens},
		{"/tokens/refresh", grokHandler.HandleAdminTokensRefresh},
		{"/tokens/refresh/async", grokHandler.HandleAdminTokensRefreshAsync},
		{"/tokens/nsfw/enable", grokHandler.HandleAdminNSFWEnable},
		{"/tokens/nsfw/enable/async", grokHandler.HandleAdminNSFWEnableAsync},
		{"/batch/", grokHandler.HandleAdminBatchTask},
		{"/cache", grokHandler.HandleAdminCache},
		{"/cache/list", grokHandler.HandleAdminCacheList},
		{"/cache/clear", grokHandler.HandleAdminCacheClear},
		{"/cache/item/delete", grokHandler.HandleAdminCacheItemDelete},
		{"/cache/online/clear", grokHandler.HandleAdminCacheOnlineClear},
		{"/cache/online/clear/async", grokHandler.HandleAdminCacheOnlineClearAsync},
		{"/cache/online/load/async", grokHandler.HandleAdminCacheOnlineLoadAsync},
		{"/voice/token", grokHandler.HandleAdminVoiceToken},
		{"/imagine/start", grokHandler.HandleAdminImagineStart},
		{"/imagine/stop", grokHandler.HandleAdminImagineStop},
		{"/imagine/sse", grokHandler.HandleAdminImagineSSE},
		{"/imagine/ws", grokHandler.HandleAdminImagineWS},
		{"/video/start", grokHandler.HandlePublicVideoStart},
		{"/video/stop", grokHandler.HandlePublicVideoStop},
		{"/video/sse", grokHandler.HandlePublicVideoSSE},
	}
	for _, rt := range adminRoutes {
		registerWithPrefixes(mux, adminPrefixes, rt.path, sessionAuth(rt.handler))
	}

	// --- Public API routes (dual prefix) ---
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

	publicPrefixes := []string{"/api/v1/public", "/v1/public"}
	publicAPIRoutes := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/verify", publicAuth(grokHandler.HandlePublicVerify)},
		{"/voice/token", publicAuth(grokHandler.HandleAdminVoiceToken)},
		{"/imagine/config", grokHandler.HandlePublicImagineConfig},
		{"/imagine/start", publicAuth(grokHandler.HandleAdminImagineStart)},
		{"/imagine/stop", publicAuth(grokHandler.HandleAdminImagineStop)},
		{"/imagine/sse", publicImagineStreamAuth(grokHandler.HandleAdminImagineSSE)},
		{"/imagine/ws", publicImagineStreamAuth(grokHandler.HandleAdminImagineWS)},
		{"/video/start", publicAuth(grokHandler.HandlePublicVideoStart)},
		{"/video/stop", publicAuth(grokHandler.HandlePublicVideoStop)},
		{"/video/sse", grokHandler.HandlePublicVideoSSE},
	}
	for _, rt := range publicAPIRoutes {
		registerWithPrefixes(mux, publicPrefixes, rt.path, rt.handler)
	}

	// --- Static assets ---
	staticRootHandler := web.StaticHandler()
	mux.Handle("/static/", http.StripPrefix("/static/", staticRootHandler))

	grokToolsURL := func() string {
		return cfg.AdminPath + "/?tab=grok-tools"
	}

	redirectToGrokTools := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, grokToolsURL(), http.StatusFound)
	}

	// --- Root + public pages ---
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if cfg.PublicAPIEnabled() {
			http.Redirect(w, r, grokToolsURL(), http.StatusFound)
			return
		}
		http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
	})
	mux.HandleFunc("/login", redirectToGrokTools)
	mux.HandleFunc("/imagine", redirectToGrokTools)
	mux.HandleFunc("/voice", redirectToGrokTools)
	mux.HandleFunc("/video", redirectToGrokTools)

	// Public page aliases (dual prefix)
	redirectPublicRoot := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !cfg.PublicAPIEnabled() {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, grokToolsURL(), http.StatusFound)
	}
	publicPagePrefixes := []string{"/v1/public", "/api/v1/public"}
	for _, prefix := range publicPagePrefixes {
		mux.HandleFunc(prefix, redirectPublicRoot)
		mux.HandleFunc(prefix+"/", redirectPublicRoot)
	}
	publicPages := []struct {
		path string
	}{
		{"/login"},
		{"/imagine"},
		{"/voice"},
		{"/video"},
	}
	for _, page := range publicPages {
		registerWithPrefixes(mux, publicPagePrefixes, page.path, redirectToGrokTools)
	}

	// --- Admin Web UI ---
	registerAdminUI(mux, cfg, s, staticRootHandler, tmplRenderer)

	// --- Health, metrics, pprof ---
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/metrics", promhttp.Handler())
	slog.Info("Prometheus metrics enabled", "path", "/metrics")

	if cfg.DebugEnabled {
		mux.HandleFunc("/debug/pprof/", middleware.SessionAuth(cfg.AdminPass, cfg.AdminToken, http.DefaultServeMux.ServeHTTP))
		slog.Info("pprof enabled", "path", "/debug/pprof/")
	}
}

func registerAdminUI(mux *http.ServeMux, cfg *config.Config, s *store.Store, staticRootHandler http.Handler, tmplRenderer *template.Renderer) {
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
		if err := tmplRenderer.RenderIndex(w, r, cfg, s); err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}

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

	for _, page := range []string{"/config", "/cache", "/token"} {
		p := page
		mux.HandleFunc(cfg.AdminPath+p, func(w http.ResponseWriter, r *http.Request) {
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

	mux.HandleFunc(cfg.AdminPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == cfg.AdminPath+"/login.html" {
			staticHandler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/css/") ||
			strings.HasPrefix(r.URL.Path, cfg.AdminPath+"/js/") {
			staticHandler.ServeHTTP(w, r)
			return
		}
		if !isAdminAuthenticated(r) {
			http.Redirect(w, r, cfg.AdminPath+"/login.html", http.StatusFound)
			return
		}
		if r.URL.Path == cfg.AdminPath+"/" || r.URL.Path == cfg.AdminPath {
			renderAdminIndex(w, r)
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
}
