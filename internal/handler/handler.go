package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	rtdebug "runtime/debug"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/orchids"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

type Handler struct {
	config       *config.Config
	client       UpstreamClient
	loadBalancer *loadbalancer.LoadBalancer
	summaryCache prompt.SummaryCache
	summaryStats *summarycache.Stats
	summaryLog   bool
	tokenCache   tokencache.Cache

	sessionWorkdirsMu sync.RWMutex
	sessionWorkdirs   map[string]string    // Map conversationKey -> string (workdir)
	sessionConvIDs    map[string]string    // Map conversationKey -> upstream warp conversationID
	sessionLastAccess map[string]time.Time // Map conversationKey -> last access time
	sessionCleanupRun time.Time

	recentReqMu      sync.Mutex
	recentRequests   map[string]*recentRequest
	recentCleanupRun time.Time
}

type UpstreamClient interface {
	SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(upstream.SSEMessage), logger *debug.Logger) error
}

type UpstreamPayloadClient interface {
	SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error
}

type ClaudeRequest struct {
	Model          string                 `json:"model"`
	Messages       []prompt.Message       `json:"messages"`
	System         SystemItems            `json:"system"`
	Tools          []interface{}          `json:"tools"`
	Stream         bool                   `json:"stream"`
	ConversationID string                 `json:"conversation_id"`
	Metadata       map[string]interface{} `json:"metadata"`
}

type toolCall struct {
	id    string
	name  string
	input string
}

const keepAliveInterval = 15 * time.Second
const maxRequestBytes = 50 * 1024 * 1024 // 50MB
const duplicateWindow = 2 * time.Second
const duplicateCleanupWindow = 10 * time.Second

type recentRequest struct {
	last     time.Time
	inFlight int
}

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	h := &Handler{
		config:            cfg,
		loadBalancer:      lb,
		sessionWorkdirs:   make(map[string]string),
		sessionConvIDs:    make(map[string]string),
		sessionLastAccess: make(map[string]time.Time),
		recentRequests:    make(map[string]*recentRequest),
	}
	if cfg != nil {
		h.summaryLog = cfg.SummaryCacheLog
		h.client = orchids.New(cfg)
	}
	return h
}

func (h *Handler) SetSummaryCache(cache prompt.SummaryCache) {
	h.summaryCache = cache
}

func (h *Handler) SetSummaryStats(stats *summarycache.Stats) {
	h.summaryStats = stats
}

func (h *Handler) SetTokenCache(cache tokencache.Cache) {
	h.tokenCache = cache
}

func (h *Handler) writeErrorResponse(w http.ResponseWriter, errType string, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

func (h *Handler) computeRequestHash(r *http.Request, body []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(r.URL.Path))
	hasher.Write([]byte{0})
	if auth := r.Header.Get("Authorization"); auth != "" {
		hasher.Write([]byte(auth))
	}
	hasher.Write([]byte{0})
	hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}

func (h *Handler) registerRequest(hash string) (bool, bool) {
	now := time.Now()
	h.recentReqMu.Lock()
	defer h.recentReqMu.Unlock()
	if h.recentRequests == nil {
		h.recentRequests = make(map[string]*recentRequest)
	}
	if rec, ok := h.recentRequests[hash]; ok {
		if now.Sub(rec.last) <= duplicateWindow {
			return true, rec.inFlight > 0
		}
		rec.last = now
		rec.inFlight++
		h.cleanupRecentLocked(now)
		return false, false
	}
	h.recentRequests[hash] = &recentRequest{last: now, inFlight: 1}
	h.cleanupRecentLocked(now)
	return false, false
}

func (h *Handler) finishRequest(hash string) {
	now := time.Now()
	h.recentReqMu.Lock()
	defer h.recentReqMu.Unlock()
	rec, ok := h.recentRequests[hash]
	if !ok {
		return
	}
	if rec.inFlight > 0 {
		rec.inFlight--
	}
	rec.last = now
	h.cleanupRecentLocked(now)
}

func (h *Handler) cleanupRecentLocked(now time.Time) {
	if len(h.recentRequests) < 256 && now.Sub(h.recentCleanupRun) < duplicateCleanupWindow {
		return
	}
	for k, rec := range h.recentRequests {
		if rec.inFlight == 0 && now.Sub(rec.last) > duplicateCleanupWindow {
			delete(h.recentRequests, k)
		}
	}
	h.recentCleanupRun = now
}

func (h *Handler) writeDuplicateResponse(w http.ResponseWriter, req ClaudeRequest) {
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		msgStart, _ := json.Marshal(map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      "dup",
				"type":    "message",
				"role":    "assistant",
				"content": []interface{}{},
				"model":   req.Model,
			},
		})
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", msgStart)
		msgStop, _ := json.Marshal(map[string]string{"type": "message_stop"})
		fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", msgStop)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"type":     "duplicate_request",
		"deduped":  true,
		"message":  "duplicate request suppressed",
		"model":    req.Model,
		"streamed": false,
	}); err != nil {
		slog.Error("Failed to write duplicate response", "error", err)
	}
}

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	streamingStarted := false

	defer func() {
		if err := recover(); err != nil {
			stack := string(rtdebug.Stack())
			slog.Error("Panic in HandleMessages", "error", err, "stack", stack)
			if streamingStarted {
				// Headers already sent — write an SSE error event instead of HTTP error
				errData, _ := json.Marshal(map[string]interface{}{
					"type": "error",
					"error": map[string]interface{}{
						"type":    "server_error",
						"message": "Internal Server Error",
					},
				})
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", errData)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				h.writeErrorResponse(w, "server_error", "Internal Server Error", http.StatusInternalServerError)
			}
		}
	}()

	if r.Method != http.MethodPost {
		h.writeErrorResponse(w, "invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	if maxRequestBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		if maxRequestBytes > 0 {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				h.writeErrorResponse(w, "invalid_request_error", "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		h.writeErrorResponse(w, "invalid_request_error", "Invalid request body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		h.writeErrorResponse(w, "invalid_request_error", "Invalid request body", http.StatusBadRequest)
		return
	}

	// 初始化调试日志
	logger := debug.New(h.config.DebugEnabled, h.config.DebugLogSSE)
	defer logger.Close()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

	reqHash := h.computeRequestHash(r, bodyBytes)
	slog.Debug("Request fingerprint", "hash", reqHash, "path", r.URL.Path, "content_length", len(bodyBytes), "retry", r.Header.Get("X-Stainless-Retry-Count"))
	if dup, inFlight := h.registerRequest(reqHash); dup {
		slog.Warn("Duplicate request suppressed", "hash", reqHash, "in_flight", inFlight, "path", r.URL.Path, "user_agent", r.UserAgent())
		logger.LogEarlyExit("duplicate_request", map[string]interface{}{
			"hash":      reqHash,
			"in_flight": inFlight,
			"path":      r.URL.Path,
		})
		h.writeDuplicateResponse(w, req)
		return
	}
	defer h.finishRequest(reqHash)

	// ...
	if ok, command := isCommandPrefixRequest(req); ok {
		slog.Debug("Handling command prefix request", "command", command)
		prefix := detectCommandPrefix(command)
		logger.LogEarlyExit("command_prefix", map[string]interface{}{
			"command": command,
			"prefix":  prefix,
		})
		writeCommandPrefixResponse(w, req, prefix, startTime, logger)
		return
	}

	cacheStrategy := h.config.CacheStrategy
	if cacheStrategy != "" && cacheStrategy != "none" {
		applyCacheStrategy(&req, cacheStrategy)
	}

	// Debug: log all headers
	for k, v := range r.Header {
		slog.Debug("Incoming header V2 CHECK", "key", k, "value", v)
	}

	// Context and Conversation Key
	conversationKey := conversationKeyForRequest(r, req)

	forcedChannel := channelFromPath(r.URL.Path)
	effectiveWorkdir, prevWorkdir, workdirChanged := h.resolveWorkdir(r, req, conversationKey)
	if workdirChanged {
		slog.Warn("检测到工作目录变化，已清空历史", "prev", prevWorkdir, "next", effectiveWorkdir, "session", conversationKey)
		req.Messages = resetMessagesForNewWorkdir(req.Messages)
		// 工作目录变化时清除上游会话ID，强制开启新对话
		if conversationKey != "" {
			h.sessionWorkdirsMu.Lock()
			delete(h.sessionConvIDs, conversationKey)
			delete(h.sessionLastAccess, conversationKey)
			h.sessionWorkdirsMu.Unlock()
		}
	}

	// 选择账号 (Initial Selection)
	failedAccountIDs := []int64{}
	failedAccountSet := make(map[int64]struct{})

	apiClient, currentAccount, err := h.selectAccount(r.Context(), req.Model, forcedChannel, failedAccountIDs)
	if err != nil {
		slog.Error("selectAccount failed", "error", err)
		logger.LogEarlyExit("select_account_failed", map[string]interface{}{
			"error":   err.Error(),
			"model":   req.Model,
			"channel": forcedChannel,
		})
		h.writeErrorResponse(w, "overloaded_error", err.Error(), http.StatusServiceUnavailable)
		return
	}
	slog.Debug("Checkpoint: selectAccount success")

	// 捕获账号快照，用于请求结束后检测 forceRefreshToken 是否更新了账号信息
	var accountSnapshot *store.Account
	if currentAccount != nil {
		snap := *currentAccount
		accountSnapshot = &snap
	}

	isWarpRequest := strings.EqualFold(forcedChannel, "warp")
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "warp") {
		isWarpRequest = true
	}
	if isWarpRequest {
		// Warp passthrough mode: do not trim history/tool results.
		slog.Debug("Checkpoint: warp passthrough, skip trim/sanitize")
	} else {
		// Orchids: Compress HUGE tool results (>100KB) to prevent upstream 413/Timeout
		// 100KB limit is generous enough for most code but prevents MB-sized payloads.
		slog.Debug("Checkpoint: compressing tool results")
		compressed, _ := compressToolResults(req.Messages, 102400, "orchids")
		req.Messages = compressed
		if sanitized, changed := sanitizeSystemItems(req.System, false, h.config); changed {
			req.System = sanitized
			slog.Info("系统提示已移除 cc_entrypoint", "mode", h.config.OrchidsCCEntrypointMode, "warp", false)
		}
	}
	slog.Debug("Checkpoint: message processing done")

	// 手动管理连接计数，账号切换时需要释放旧账号、获取新账号
	trackedAccountID := int64(0)
	if currentAccount != nil && h.loadBalancer != nil {
		h.loadBalancer.AcquireConnection(currentAccount.ID)
		trackedAccountID = currentAccount.ID
	}
	defer func() {
		if trackedAccountID != 0 && h.loadBalancer != nil {
			h.loadBalancer.ReleaseConnection(trackedAccountID)
		}
	}()

	var hitsBefore, missesBefore uint64
	if h.summaryStats != nil && h.summaryLog {
		hitsBefore, missesBefore = h.summaryStats.Snapshot()
	}

	suggestionMode := isSuggestionMode(req.Messages)
	noThinking := suggestionMode || h.config.SuppressThinking
	gateNoTools := false
	suppressThinking := noThinking
	if suggestionMode {
		gateNoTools = true
	}
	effectiveTools := req.Tools
	if h.config.WarpDisableTools != nil && *h.config.WarpDisableTools {
		effectiveTools = nil
	}
	if gateNoTools {
		effectiveTools = nil
		slog.Debug("tool_gate: disabled tools for short non-code request")
	}

	// 构建 prompt（V2 Markdown 格式）
	startBuild := time.Now()

	summaryKey := conversationKey
	if strings.TrimSpace(effectiveWorkdir) != "" {
		summaryKey = conversationKey + "|" + strings.TrimSpace(effectiveWorkdir)
	}
	opts := prompt.PromptOptions{
		Context:          r.Context(),
		ConversationID:   summaryKey,
		MaxTokens:        h.config.ContextMaxTokens,
		SummaryMaxTokens: h.config.ContextSummaryMaxTokens,
		KeepTurns:        h.config.ContextKeepTurns,
		SummaryCache:     h.summaryCache,
		ProjectRoot:      effectiveWorkdir,
	}

	slog.Debug("Starting prompt build...", "conversation_id", conversationKey)
	isOrchidsAIClient := false
	if _, ok := apiClient.(*orchids.Client); ok && strings.EqualFold(strings.TrimSpace(h.config.OrchidsImpl), "aiclient") {
		isOrchidsAIClient = true
	}

	var aiClientHistory []map[string]string
	var builtPrompt string
	if isOrchidsAIClient {
		builtPrompt, aiClientHistory = orchids.BuildAIClientPromptAndHistory(req.Messages, req.System, req.Model, noThinking, effectiveWorkdir)
	} else {
		builtPrompt = prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
			Model:    req.Model,
			Messages: req.Messages,
			System:   req.System,
			Tools:    effectiveTools,
			Stream:   req.Stream,
		}, opts)
	}
	buildDuration := time.Since(startBuild)
	slog.Debug("Prompt build completed", "duration", buildDuration)
	if h.config.DebugEnabled {
		buildLabel := "BuildPromptV2WithOptions"
		if isOrchidsAIClient {
			buildLabel = "BuildAIClientPromptAndHistory"
		}
		slog.Info("[Performance] "+buildLabel, "duration", buildDuration)
		if opts.ProjectContext != "" {
			slog.Debug("Project context injected", "context", opts.ProjectContext)
		}
	}

	if h.summaryStats != nil && h.summaryLog {
		hitsAfter, missesAfter := h.summaryStats.Snapshot()
		hitDelta := hitsAfter - hitsBefore
		missDelta := missesAfter - missesBefore
		if hitDelta > 0 || missDelta > 0 {
			slog.Debug("summary_cache", "hit", hitDelta, "miss", missDelta)
		}
	}

	// 映射模型
	mappedModel := mapModel(req.Model)
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "warp") {
		mappedModel = req.Model
	}
	slog.Info("Model mapping", "original", req.Model, "mapped", mappedModel)

	isStream := req.Stream

	if isStream {
		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		streamingStarted = true

		if _, ok := w.(http.Flusher); !ok {
			h.writeErrorResponse(w, "api_error", "Streaming not supported by underlying connection", http.StatusInternalServerError)
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// 状态管理
	// msgID is now managed by streamHandler

	var chatHistory []interface{}
	upstreamMessages := append([]prompt.Message(nil), req.Messages...)

	// Pre-allocate chatHistory
	if isOrchidsAIClient {
		chatHistory = make([]interface{}, 0, len(aiClientHistory))
		for _, item := range aiClientHistory {
			chatHistory = append(chatHistory, item)
		}
	} else {
		chatHistory = make([]interface{}, 0, 10)
	}

	if gateNoTools {
		builtPrompt = injectToolGate(builtPrompt, "This is a short, non-code request. Do NOT call tools or perform any file operations. Answer directly.")
	}

	// 2. 记录转换后的 prompt
	slog.Debug("Checkpoint: LogConvertedPrompt")
	logger.LogConvertedPrompt(builtPrompt)

	// Token 计数
	inputTokens := h.estimateInputTokens(r.Context(), req.Model, builtPrompt)

	// Detect Response Format (Anthropic vs OpenAI)
	responseFormat := adapter.DetectResponseFormat(r.URL.Path)

	sh := newStreamHandler(
		h.config, w, logger, suppressThinking, isStream, responseFormat, effectiveWorkdir,
	)
	sh.seedSideEffectDedupFromMessages(upstreamMessages)
	sh.setUsageTokens(inputTokens, -1) // Correctly initialize input tokens
	// 捕获上游返回的 conversationID，持久化到 session 以便后续请求复用
	sh.onConversationID = func(id string) {
		if conversationKey == "" {
			return
		}
		h.sessionWorkdirsMu.Lock()
		h.sessionConvIDs[conversationKey] = id
		h.sessionLastAccess[conversationKey] = time.Now()
		h.cleanupSessionWorkdirsLocked()
		h.sessionWorkdirsMu.Unlock()
		slog.Debug("Warp conversationID captured", "key", conversationKey, "id", id)
	}
	defer sh.release()

	// 发送 message_start
	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      sh.msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   req.Model,
			"usage":   map[string]int{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
	sh.writeSSE("message_start", string(startData))

	slog.Debug("New request received")

	// KeepAlive
	var keepAliveStop chan struct{}
	if isStream {
		keepAliveStop = make(chan struct{})
		defer close(keepAliveStop)
		ticker := time.NewTicker(keepAliveInterval)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sh.mu.Lock()
					done := sh.hasReturn
					sh.mu.Unlock()
					if done {
						return
					}
					sh.writeKeepAlive()
				case <-keepAliveStop:
					return
				case <-r.Context().Done():
					return
				}
			}
		}()
	}

	// Main execution
	run := func() {
		// 复用上游返回的 conversationID，保持会话连续性
		chatSessionID := ""
		if conversationKey != "" {
			h.sessionWorkdirsMu.Lock()
			chatSessionID = h.sessionConvIDs[conversationKey]
			h.sessionLastAccess[conversationKey] = time.Now()
			h.cleanupSessionWorkdirsLocked()
			h.sessionWorkdirsMu.Unlock()
		}
		if chatSessionID == "" {
			chatSessionID = "chat_" + randomSessionID()
		}
		maxRetries := h.config.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		retryDelay := time.Duration(h.config.RetryDelay) * time.Millisecond
		retriesRemaining := maxRetries

		payloadMessages := upstreamMessages
		payloadSystem := req.System

		upstreamReq := upstream.UpstreamRequest{
			Prompt:        builtPrompt,
			ChatHistory:   chatHistory,
			Workdir:       effectiveWorkdir,
			Model:         mappedModel,
			Messages:      payloadMessages,
			System:        payloadSystem,
			Tools:         effectiveTools,
			NoTools:       gateNoTools,
			NoThinking:    noThinking,
			ChatSessionID: chatSessionID,
		}
		for {
			if retriesRemaining < maxRetries {
				// 非首次尝试：向客户端发送重试提示，避免前一次不完整内容造成混淆
				sh.emitTextBlock("\n\n[Retrying request...]\n\n")
			}
			sh.resetRoundState()
			var err error
			slog.Debug("Calling Upstream Client...", "attempt", maxRetries-retriesRemaining+1)

			slog.Info("Interface check", "type", fmt.Sprintf("%T", apiClient))
			if sender, ok := apiClient.(UpstreamPayloadClient); ok {
				slog.Info("Using SendRequestWithPayload")
				warpBatches := [][]prompt.Message{upstreamMessages}
				if isWarpRequest && h.config.WarpSplitToolResults {
					if _, isWarp := apiClient.(*warp.Client); isWarp {
						batches, total := splitWarpToolResults(upstreamMessages, 1)
						if len(batches) > 1 {
							slog.Info("Warp 工具结果分批发送", "total_tool_results", total, "batches", len(batches))
						}
						warpBatches = batches
					}
				}
				noopHandler := func(msg upstream.SSEMessage) {
					if msg.Type == "error" {
						slog.Warn("Warp intermediate batch error", "event", msg.Event)
					}
				}
				for i, batch := range warpBatches {
					batchReq := upstreamReq
					batchReq.Messages = batch
					isLast := i == len(warpBatches)-1
					if isLast {
						err = sender.SendRequestWithPayload(r.Context(), batchReq, sh.handleMessage, logger)
					} else {
						err = sender.SendRequestWithPayload(r.Context(), batchReq, noopHandler, nil)
					}
					if err != nil {
						break
					}
				}
			} else {
				slog.Warn("Falling back to legacy SendRequest (Workdir lost!)", "type", fmt.Sprintf("%T", apiClient))
				err = apiClient.SendRequest(r.Context(), builtPrompt, chatHistory, mappedModel, sh.handleMessage, logger)
			}
			slog.Debug("Upstream Client Returned", "error", err)

			if err == nil {
				sh.forceFinishIfMissing()
				break
			}

			// Check for non-retriable errors
			errStr := err.Error()
			errClass := classifyUpstreamError(errStr)
			slog.Error("Request error", "error", err, "category", errClass.category, "retryable", errClass.retryable)
			// 标记账号状态（auth 类错误始终标记，无论是否可重试）
			if currentAccount != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
				if status := classifyAccountStatus(errStr); status != "" {
					// Mark status if it's auth-related OR if it's a 429 (rate limit)
					// We want to rotate accounts on 429 even if we retry the request on a new account
					if !errClass.retryable || errClass.category == "auth" || status == "429" {
						slog.Info("标记账号状态", "account_id", currentAccount.ID, "status", status, "category", errClass.category)
						markAccountStatus(r.Context(), h.loadBalancer.Store, currentAccount, status)
					}
				}
			}

			if !errClass.retryable {
				slog.Error("Aborting retries for non-retriable error", "error", err, "category", errClass.category)
				if errClass.category == "auth_blocked" || errClass.category == "auth" {
					sh.InjectAuthError(errClass.category, errStr)
				}
				sh.finishResponse("end_turn")
				return
			}

			if r.Context().Err() != nil {
				sh.finishResponse("end_turn")
				return
			}
			if retriesRemaining <= 0 {
				if currentAccount != nil && h.loadBalancer != nil {
					slog.Error("Account request failed, max retries reached", "account", currentAccount.Name)
				}
				if errClass.category == "auth" || errClass.category == "auth_blocked" {
					sh.InjectAuthError(errClass.category, errStr)
				} else {
					sh.InjectRetryExhaustedError(errStr)
				}
				sh.finishResponse("end_turn")
				return
			}
			retriesRemaining--
			if errClass.switchAccount && currentAccount != nil && h.loadBalancer != nil {
				if _, ok := failedAccountSet[currentAccount.ID]; !ok {
					failedAccountSet[currentAccount.ID] = struct{}{}
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
				}
				slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "unsuccessful_attempts", len(failedAccountIDs))

				// 释放旧账号的连接计数
				if trackedAccountID != 0 {
					h.loadBalancer.ReleaseConnection(trackedAccountID)
					trackedAccountID = 0
				}

				var retryErr error
				apiClient, currentAccount, retryErr = h.selectAccount(r.Context(), req.Model, forcedChannel, failedAccountIDs)
				if retryErr == nil {
					if currentAccount != nil {
						h.loadBalancer.AcquireConnection(currentAccount.ID)
						trackedAccountID = currentAccount.ID
						slog.Debug("Switched to account", "account", currentAccount.Name)
					} else {
						slog.Debug("Switched to default upstream config")
					}
				} else {
					slog.Error("No more accounts available", "error", retryErr)
					sh.InjectNoAvailableAccountError(errStr, retryErr)
					sh.finishResponse("end_turn")
					return
				}
			}
			if retryDelay > 0 {
				attempt := maxRetries - retriesRemaining + 1
				delay := computeRetryDelay(retryDelay, attempt, errClass.category)
				if delay > 0 && !util.SleepWithContext(r.Context(), delay) {
					sh.finishResponse("end_turn")
					return
				}
			}
		}
	}

	run()

	// 确保有最终响应
	if !sh.hasReturn {
		sh.finishResponse("end_turn")
	}

	if !isStream {
		stopReason := sh.finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}

		for i := range sh.contentBlocks {
			blockType, _ := sh.contentBlocks[i]["type"].(string)
			switch blockType {
			case "text":
				if builder, ok := sh.textBlockBuilders[i]; ok {
					sh.contentBlocks[i]["text"] = builder.String()
				} else if _, ok := sh.contentBlocks[i]["text"]; !ok {
					sh.contentBlocks[i]["text"] = ""
				}
			case "thinking":
				if builder, ok := sh.thinkingBlockBuilders[i]; ok {
					sh.contentBlocks[i]["thinking"] = builder.String()
				} else if _, ok := sh.contentBlocks[i]["thinking"]; !ok {
					sh.contentBlocks[i]["thinking"] = ""
				}
			}
		}

		if len(sh.contentBlocks) == 0 && sh.responseText.Len() > 0 {
			sh.contentBlocks = append(sh.contentBlocks, map[string]interface{}{
				"type": "text",
				"text": sh.responseText.String(),
			})
		}

		response := map[string]interface{}{
			"id":            sh.msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       sh.contentBlocks,
			"model":         req.Model,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  sh.inputTokens,
				"output_tokens": sh.outputTokens,
			},
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("Failed to write JSON response", "error", err)
		}

	}

	// Sync state and update stats using helpers
	h.syncWarpState(currentAccount, apiClient, accountSnapshot)
	h.updateAccountStats(currentAccount, sh.inputTokens, sh.outputTokens)
}

func randomSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails (unlikely)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
