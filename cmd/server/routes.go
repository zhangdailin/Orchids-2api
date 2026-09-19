package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/api"
	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/warp"
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
	rootMux *http.ServeMux,
	cfg *config.Config,
	s *store.Store,
	h *handler.Handler,
	grokHandler *grok.Handler,
	apiHandler *api.API,
	limiter *middleware.ConcurrencyLimiter,
	accountTracker loadbalancer.ConnTracker,
	tmplRenderer *template.Renderer,
) {
	mux := http.NewServeMux()
	currentConfig := func() *config.Config {
		if current := apiHandler.ConfigSnapshot(); current != nil {
			return current
		}
		return cfg
	}
	inferenceAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return middleware.APIKeyAuth(
			func() bool { return currentConfig().InferenceAuthEnabled() },
			func(ctx context.Context, token string) (*middleware.APIKeyPrincipal, error) {
				key, err := s.AuthorizeApiKey(ctx, token)
				switch {
				case err == nil:
					return &middleware.APIKeyPrincipal{ID: key.ID, AllowedModels: key.AllowedModels, MaxConcurrent: key.MaxConcurrent}, nil
				case err == store.ErrNoRows:
					return nil, nil
				case err == store.ErrApiKeyExpired:
					return &middleware.APIKeyPrincipal{DenialCode: middleware.APIKeyDenialExpired}, nil
				case err == store.ErrApiKeyRateLimited:
					return &middleware.APIKeyPrincipal{DenialCode: middleware.APIKeyDenialRateLimited}, nil
				default:
					return nil, err
				}
			},
			middleware.APIKeyConcurrencyWithTracker(next, accountTracker),
		)
	}
	// channelPrefixes are the channels that share the generic Anthropic and
	// OpenAI handlers. Grok is not among them: it has a native implementation of
	// both. /v1 is excluded too, because it is the unified prefix — it dispatches
	// by model instead of by path.
	channelPrefixes := []string{"/warp/v1", "/puter/v1", "/workbuddy/v1", "/qoder/v1"}
	// allPrefixes additionally serves the native Grok prefix and the unified one.
	// It is for the routes whose answer comes from shared state and is the same
	// whichever prefix carried the request.
	allPrefixes := make([]string, 0, len(channelPrefixes)+2)
	allPrefixes = append(allPrefixes, channelPrefixes...)
	allPrefixes = append(allPrefixes, "/grok/v1", "/v1")

	// --- Channel-specific message routes ---
	// Every channel answers the same two endpoints; the path only tells the
	// handler which channel's token profile and account pool to use.
	registerWithPrefixes(mux, channelPrefixes, "/messages", inferenceAuth(limiter.Limit(h.HandleMessages)))
	registerWithPrefixes(mux, channelPrefixes, "/messages/count_tokens", inferenceAuth(limiter.Limit(h.HandleCountTokens)))

	// --- Model routes (channel prefixes → same handlers) ---
	registerWithPrefixes(mux, allPrefixes, "/models", inferenceAuth(h.HandleModels))
	registerWithPrefixes(mux, allPrefixes, "/models/", inferenceAuth(h.HandleModelByID))

	// --- OpenAI-compatible chat/image routes (channel-specific + unified) ---
	registerWithPrefixes(mux, channelPrefixes, "/chat/completions", inferenceAuth(limiter.Limit(h.HandleMessages)))

	// --- OpenAI Responses API for the chat-completions-only channels ---
	// Codex defaults to the Responses wire API, so without this bridge every
	// channel except Grok answers 404 on /responses. The bridge forwards to the
	// same channel's chat handler, which keeps account selection and retries in
	// one place. Records are persisted in the shared response store, so
	// store=true, previous_response_id and GET/DELETE /responses/{id} work here
	// too. The subtree below /responses/ carries the sibling endpoints
	// (trailing slash, compact, resource retrieval).
	responseStoreTTL := time.Duration(0)
	if cfg != nil && cfg.ResponseStoreTTL > 0 {
		responseStoreTTL = time.Duration(cfg.ResponseStoreTTL) * time.Hour
	}
	bridgeOptions := grok.ResponsesBridgeOptions{Store: s, TTL: responseStoreTTL}
	channelResponses := grok.ResponsesBridgeHandler(h.HandleMessages, bridgeOptions)
	channelResponsesSub := grok.ResponsesChannelSubpath(h.HandleMessages, bridgeOptions)
	registerWithPrefixes(mux, channelPrefixes, "/responses", inferenceAuth(limiter.Limit(channelResponses)))
	registerWithPrefixes(mux, channelPrefixes, "/responses/", inferenceAuth(limiter.Limit(channelResponsesSub)))
	// The sibling endpoints below a response id are registered explicitly on
	// every prefix. Going through the model dispatcher would route them by the
	// body, and a cancel body carries no model: the same request would land on
	// the native handler or the bridge depending on whether the client sent `{}`
	// or nothing at all. Both answers come from the shared response store, so
	// they are the same implementation whichever channel wrote the record.
	registerWithPrefixes(mux, allPrefixes, "/responses/{response_id}/cancel",
		inferenceAuth(limiter.Limit(grok.ResponsesCancelHandler(bridgeOptions))))
	registerWithPrefixes(mux, allPrefixes, "/responses/{response_id}/input_items",
		inferenceAuth(limiter.Limit(grok.ResponsesInputItemsHandler(bridgeOptions))))

	grokPrefixes := []string{"/grok/v1"}
	registerWithPrefixes(mux, grokPrefixes, "/chat/completions", inferenceAuth(limiter.Limit(grokHandler.HandleChatCompletions)))
	registerWithPrefixes(mux, grokPrefixes, "/messages", inferenceAuth(limiter.Limit(grokHandler.HandleMessages)))
	// /grok/v1 keeps the native Responses implementation; the unified /v1
	// prefix dispatches by model so a Codex client can point at one base URL.
	registerWithPrefixes(mux, grokPrefixes, "/responses", inferenceAuth(limiter.Limit(grokHandler.HandleResponses)))
	registerWithPrefixes(mux, grokPrefixes, "/responses/compact", inferenceAuth(limiter.Limit(grokHandler.HandleResponsesCompact)))
	registerWithPrefixes(mux, grokPrefixes, "/responses/", inferenceAuth(limiter.Limit(grokHandler.HandleResponseResource)))
	isNativeResponsesModel := func(ctx context.Context, model string) (bool, error) {
		if _, ok := grok.ResolveModel(model); ok {
			return true, nil
		}
		// The channel lookup is a store read: give it the request's lifetime so a
		// slow or unavailable Redis cannot pin a request thread, and report the
		// failure instead of silently answering "not native" or "native".
		channel, err := h.LookupChannelForModel(ctx, model)
		if err != nil {
			slog.Warn("Unified route channel lookup failed; using the bridged handler", "model", model, "error", err)
			return false, err
		}
		return strings.EqualFold(channel, "grok"), nil
	}
	nativeResponsesSub := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/responses/compact") {
			grokHandler.HandleResponsesCompact(w, r)
			return
		}
		grokHandler.HandleResponseResource(w, r)
	}
	// The unified prefix serves every channel's models, so it must not be owned
	// by one provider: Grok models keep their native handlers, everything else
	// goes through the shared pipeline, which picks the channel from the model.
	mux.HandleFunc("/v1/chat/completions", inferenceAuth(limiter.Limit(grok.ModelDispatcher(grokHandler.HandleChatCompletions, h.HandleMessages, isNativeResponsesModel))))
	mux.HandleFunc("/v1/messages", inferenceAuth(limiter.Limit(grok.ModelDispatcher(grokHandler.HandleMessages, h.HandleMessages, isNativeResponsesModel))))
	mux.HandleFunc("/v1/responses", inferenceAuth(limiter.Limit(grok.ModelDispatcher(grokHandler.HandleResponses, channelResponses, isNativeResponsesModel))))
	// POST is dispatched by model, but GET and DELETE have no model to read: the
	// stored record decides instead, so a bridged record is served by the bridge
	// that wrote it rather than by Grok's handler accepting a foreign record.
	mux.HandleFunc("/v1/responses/", inferenceAuth(limiter.Limit(grok.ResponsesUnifiedResource(nativeResponsesSub, bridgeOptions))))
	// count_tokens takes the same dispatch decision as the request it precedes:
	// /warp/v1 and /puter/v1 have their own token profiles, and a client that
	// counts against one channel while the completion runs on another plans its
	// context against the wrong number. On the unified prefix the channel is the
	// model's, not the path's.
	mux.HandleFunc("/v1/messages/count_tokens", inferenceAuth(limiter.Limit(grok.ModelDispatcher(h.HandleCountTokens, h.HandleCountTokens, isNativeResponsesModel))))
	registerWithPrefixes(mux, allPrefixes, "/images/generations", inferenceAuth(limiter.Limit(grokHandler.HandleImagesGenerations)))
	registerWithPrefixes(mux, allPrefixes, "/images/edits", inferenceAuth(limiter.Limit(grokHandler.HandleImagesEdits)))
	registerWithPrefixes(mux, allPrefixes, "/videos", inferenceAuth(limiter.Limit(grokHandler.HandleVideosCreate)))
	registerWithPrefixes(mux, allPrefixes, "/videos/generations", inferenceAuth(limiter.Limit(grokHandler.HandleConsoleVideosGenerate)))
	registerWithPrefixes(mux, allPrefixes, "/videos/edits", inferenceAuth(limiter.Limit(grokHandler.HandleConsoleVideosEdit)))
	registerWithPrefixes(mux, allPrefixes, "/videos/extensions", inferenceAuth(limiter.Limit(grokHandler.HandleConsoleVideosExtend)))
	registerWithPrefixes(mux, allPrefixes, "/videos/", inferenceAuth(limiter.Limit(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/content") {
			grokHandler.HandleVideosContent(w, r)
			return
		}
		grokHandler.HandleVideosRetrieve(w, r)
	})))
	registerWithPrefixes(mux, allPrefixes, "/files/", inferenceAuth(grokHandler.HandleFiles))
	registerWithPrefixes(mux, allPrefixes, "/media/inputs", inferenceAuth(limiter.Limit(grokHandler.HandleMediaInputs)))
	registerWithPrefixes(mux, allPrefixes, "/media/inputs/", inferenceAuth(limiter.Limit(grokHandler.HandleMediaInputResource)))
	// One-time, unguessable callback used by the xAI video fallback. The token
	// is the authorization boundary, so this endpoint must not require a client key.
	mux.HandleFunc("/media/uploads/", grokHandler.HandleVideoUpload)
	registerWithPrefixes(mux, allPrefixes, "/tts", inferenceAuth(limiter.Limit(grokHandler.HandleTTS)))
	registerWithPrefixes(mux, allPrefixes, "/tts/voices", inferenceAuth(limiter.Limit(grokHandler.HandleTTSVoices)))
	registerWithPrefixes(mux, allPrefixes, "/tts/voices/", inferenceAuth(limiter.Limit(grokHandler.HandleTTSVoices)))
	sttHTTP := limiter.Limit(grokHandler.HandleSTT)
	sttWebSocket := limiter.LimitLongLived(grokHandler.HandleSTT)
	registerWithPrefixes(mux, allPrefixes, "/stt", inferenceAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			sttWebSocket(w, r)
			return
		}
		sttHTTP(w, r)
	}))
	registerWithPrefixes(mux, allPrefixes, "/audio/speech", inferenceAuth(limiter.Limit(grokHandler.HandleAudioSpeech)))
	registerWithPrefixes(mux, allPrefixes, "/audio/tasks", inferenceAuth(limiter.Limit(grokHandler.HandleAudioSpeech)))
	registerWithPrefixes(mux, allPrefixes, "/audio/transcriptions", inferenceAuth(limiter.Limit(grokHandler.HandleAudioTranscriptions)))
	registerWithPrefixes(mux, allPrefixes, "/realtime", inferenceAuth(limiter.LimitLongLived(grokHandler.HandleRealtime)))

	// --- Public auth/login (no prefix duplication) ---
	mux.HandleFunc("/api/login", apiHandler.HandleLogin)
	mux.HandleFunc("/api/logout", apiHandler.HandleLogout)

	// --- Admin API routes (session auth, dual prefix) ---
	sessionAuth := func(h http.HandlerFunc) http.HandlerFunc {
		return middleware.SessionAuthDynamic(func() (string, string) {
			current := currentConfig()
			return current.AdminPass, current.AdminToken
		}, h)
	}

	// Admin routes under /api/* only (no dual prefix)
	mux.HandleFunc("/api/accounts", sessionAuth(apiHandler.HandleAccounts))
	mux.HandleFunc("/api/accounts/", sessionAuth(apiHandler.HandleAccountByID))
	mux.HandleFunc("/api/grok/availability", sessionAuth(apiHandler.HandleGrokAvailability))
	mux.HandleFunc("/api/puter/web-login", sessionAuth(apiHandler.HandlePuterWebLogin))
	mux.HandleFunc("/api/workbuddy/login", sessionAuth(apiHandler.HandleWorkBuddyLogin))
	mux.HandleFunc("/api/workbuddy/login/", sessionAuth(apiHandler.HandleWorkBuddyLogin))
	mux.HandleFunc("/api/qoder/login", sessionAuth(apiHandler.HandleQoderLogin))
	mux.HandleFunc("/api/qoder/login/", sessionAuth(apiHandler.HandleQoderLogin))
	mux.HandleFunc("/api/warp/device-auth", sessionAuth(apiHandler.HandleWarpDeviceAuthorization))
	mux.HandleFunc("/api/warp/device-auth/", sessionAuth(apiHandler.HandleWarpDeviceAuthorization))
	mux.HandleFunc("/api/grok/device-auth", sessionAuth(apiHandler.HandleGrokDeviceAuthorization))
	mux.HandleFunc("/api/grok/device-auth/", sessionAuth(apiHandler.HandleGrokDeviceAuthorization))
	mux.HandleFunc("/api/keys", sessionAuth(apiHandler.HandleKeys))
	mux.HandleFunc("/api/keys/", sessionAuth(apiHandler.HandleKeyByID))
	mux.HandleFunc("/api/models", sessionAuth(apiHandler.HandleModels))
	mux.HandleFunc("/api/models/refresh", sessionAuth(func(w http.ResponseWriter, r *http.Request) {
		makeModelRefreshHandler(currentConfig(), s)(w, r)
	}))
	mux.HandleFunc("/api/models/", sessionAuth(apiHandler.HandleModelByID))
	mux.HandleFunc("/api/export", sessionAuth(apiHandler.HandleExport))
	mux.HandleFunc("/api/import", sessionAuth(apiHandler.HandleImport))
	mux.HandleFunc("/api/config", sessionAuth(apiHandler.HandleConfig))
	mux.HandleFunc("/api/config/list", sessionAuth(apiHandler.HandleConfigList))
	mux.HandleFunc("/api/config/save", sessionAuth(apiHandler.HandleConfigSave))
	mux.HandleFunc("/api/config/cache/clear", sessionAuth(apiHandler.HandleCacheClear))
	mux.HandleFunc("/api/token-cache/stats", sessionAuth(apiHandler.HandleTokenCacheStats))
	mux.HandleFunc("/api/token-cache/clear", sessionAuth(apiHandler.HandleTokenCacheClear))
	mux.HandleFunc("/api/audit", sessionAuth(apiHandler.HandleAuditEvents))
	// Operations monitoring: the overview, the channel × model matrix and the
	// alert set behind the 运维总览 page.
	mux.HandleFunc("/api/ops/overview", sessionAuth(apiHandler.HandleOpsOverview))
	mux.HandleFunc("/api/ops/channels", sessionAuth(apiHandler.HandleOpsChannels))
	mux.HandleFunc("/api/ops/alerts", sessionAuth(apiHandler.HandleOpsAlerts))
	mux.HandleFunc("/api/ops/alerts/rules", sessionAuth(apiHandler.HandleOpsAlertRules))
	mux.HandleFunc("/api/ops/runtime", sessionAuth(apiHandler.HandleOpsRuntime))
	// Journal: one endpoint per tab (request / operation / system) with the
	// upstream attempts of each request joined in.
	mux.HandleFunc("/api/journal/records", sessionAuth(apiHandler.HandleJournalRecords))
	mux.HandleFunc("/api/journal/diagnostics", sessionAuth(apiHandler.HandleJournalDiagnostics))
	mux.HandleFunc("/api/journal/diagnostics/settings", sessionAuth(apiHandler.HandleDiagnosticSettings))
	mux.HandleFunc("/api/journal/operations", sessionAuth(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("kind") == "" {
			query.Set("kind", "operation")
			r.URL.RawQuery = query.Encode()
		}
		apiHandler.HandleJournalRecords(w, r)
	}))
	mux.HandleFunc("/api/journal/system", sessionAuth(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("kind") == "" {
			query.Set("kind", "system")
			r.URL.RawQuery = query.Encode()
		}
		apiHandler.HandleJournalRecords(w, r)
	}))

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
			middleware.PublicKeyAuth(currentConfig().PublicAPIKey(), next)(w, r)
		}
	}
	publicImagineStreamAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			middleware.PublicImagineStreamAuth(currentConfig().PublicAPIKey(), next)(w, r)
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
		if currentConfig().PublicAPIEnabled() {
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
		if !currentConfig().PublicAPIEnabled() {
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
	publicPages := []string{"/login", "/imagine", "/voice", "/video"}
	for _, page := range publicPages {
		registerWithPrefixes(mux, publicPagePrefixes, page, redirectToGrokTools)
	}

	// --- Admin Web UI ---
	registerAdminUI(mux, cfg, currentConfig, s, staticRootHandler, tmplRenderer)

	// --- Health, metrics, pprof ---
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		warpStatus := "ready"
		if warp.ConfigurationError() != nil {
			status = "degraded"
			warpStatus = "configuration_error"
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    status,
			"providers": map[string]string{"warp": warpStatus},
		})
	})
	mux.HandleFunc("/ready/warp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := warp.ConfigurationError(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "configuration_error", "message": "Warp OAuth is not configured"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})
	mux.Handle("/metrics", promhttp.Handler())
	slog.Debug("Prometheus metrics enabled", "path", "/metrics")

	if cfg.DebugEnabled {
		mux.HandleFunc("/debug/pprof/", middleware.SessionAuthDynamic(func() (string, string) {
			current := currentConfig()
			return current.AdminPass, current.AdminToken
		}, http.DefaultServeMux.ServeHTTP))
		slog.Debug("pprof enabled", "path", "/debug/pprof/")
	}
	// Guard every /v1 path, including aliases, media and unknown endpoints.
	// Registered inference routes reuse the validated principal without charging
	// their key's RPM budget or concurrency slot twice.
	v1Guard := inferenceAuth(mux.ServeHTTP)
	rootMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
			v1Guard(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func registerAdminUI(mux *http.ServeMux, cfg *config.Config, currentConfig func() *config.Config, s *store.Store, staticRootHandler http.Handler, tmplRenderer *template.Renderer) {
	staticHandler := http.StripPrefix(cfg.AdminPath, staticRootHandler)
	currentUIConfig := func() *config.Config {
		current := currentConfig().Clone()
		// The route tree cannot be re-registered while the server is running.
		// AdminPath therefore remains a restart-required setting even though the
		// rest of the rendered configuration is refreshed immediately.
		current.AdminPath = cfg.AdminPath
		return current
	}

	isAdminAuthenticated := func(r *http.Request) bool {
		cookie, err := r.Cookie("session_token")
		authenticated := err == nil && auth.ValidateSessionToken(cookie.Value)
		if authenticated {
			return true
		}
		adminToken := currentConfig().AdminToken
		authHeader := r.Header.Get("Authorization")
		return adminToken != "" && (authHeader == "Bearer "+adminToken || authHeader == adminToken || r.Header.Get("X-Admin-Token") == adminToken)
	}
	renderAdminIndex := func(w http.ResponseWriter, r *http.Request) {
		if err := tmplRenderer.RenderIndex(w, r, currentUIConfig(), s); err != nil {
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
		mux.HandleFunc(cfg.AdminPath+page, func(w http.ResponseWriter, r *http.Request) {
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
