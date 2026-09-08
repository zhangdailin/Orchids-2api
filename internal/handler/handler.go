package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	rtdebug "runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

// ClientFactory creates an upstream client for a given account.
// Used to decouple provider-specific client construction from the handler.
type ClientFactory func(acc *store.Account, cfg *config.Config) UpstreamClient

type Handler struct {
	config        *config.Config
	client        UpstreamClient
	clientFactory ClientFactory
	clientCache   *accountClientCache
	loadBalancer  *loadbalancer.LoadBalancer
	connTracker   loadbalancer.ConnTracker
	tokenCache    tokencache.Cache
	promptCache   tokencache.PromptCache
	auditLogger   audit.Logger

	sessionStore SessionStore
	// Coalesces upstream model-config refresh signals per Warp account.
	warpModelRefreshes sync.Map
}

type UpstreamClient interface {
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

type openAINonStreamToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAINonStreamMessage struct {
	Role             string                    `json:"role"`
	Content          interface{}               `json:"content"`
	ReasoningContent string                    `json:"reasoning_content,omitempty"`
	ToolCalls        []openAINonStreamToolCall `json:"tool_calls,omitempty"`
}

type openAINonStreamChoice struct {
	Index        int                    `json:"index"`
	Message      openAINonStreamMessage `json:"message"`
	FinishReason *string                `json:"finish_reason"`
}

type openAINonStreamUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAINonStreamResponse struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []openAINonStreamChoice `json:"choices"`
	Usage   openAINonStreamUsage    `json:"usage"`
}

const keepAliveInterval = 15 * time.Second
const maxRequestBytes = 50 * 1024 * 1024 // 50MB

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	h := &Handler{
		config:       cfg,
		loadBalancer: lb,
		connTracker:  loadbalancer.NewMemoryConnTracker(),
		clientCache:  newAccountClientCache(),
		sessionStore: NewMemorySessionStore(30*time.Minute, 1024),
		auditLogger:  audit.NewNopLogger(),
	}

	return h
}

func (h *Handler) SetTokenCache(cache tokencache.Cache) {
	h.tokenCache = cache
}

func (h *Handler) SetPromptCache(cache tokencache.PromptCache) {
	h.promptCache = cache
}

// SetSessionStore replaces the default in-memory session store.
func (h *Handler) SetSessionStore(ss SessionStore) {
	h.sessionStore = ss
}

// SetAuditLogger replaces the default nop audit logger.
func (h *Handler) SetAuditLogger(al audit.Logger) {
	h.auditLogger = al
}

// SetClientFactory sets the factory used by selectAccount to create provider-specific clients.
func (h *Handler) SetClientFactory(f ClientFactory) {
	h.clientFactory = f
}

func (h *Handler) computeRequestHash(r *http.Request, body []byte) string {
	hasher := sha256.New()
	hasher.Write([]byte(r.URL.Path))
	hasher.Write([]byte{0})
	for _, identity := range []string{middleware.APIKeyFingerprint(r.Context()), r.Header.Get("Authorization"), r.Header.Get("X-API-Key")} {
		hasher.Write([]byte(identity))
		hasher.Write([]byte{0})
	}
	hasher.Write([]byte{0})
	hasher.Write(body)
	return hex.EncodeToString(hasher.Sum(nil))
}

func mapStopReasonToOpenAIFinishReason(stopReason string) *string {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn", "stop":
		reason := "stop"
		return &reason
	case "tool_use":
		reason := "tool_calls"
		return &reason
	case "max_tokens":
		reason := "length"
		return &reason
	case "refusal":
		reason := "content_filter"
		return &reason
	default:
		reason := stopReason
		return &reason
	}
}

func buildOpenAINonStreamResponse(sh *streamHandler, model string, stopReason string) openAINonStreamResponse {
	textParts := make([]string, 0, len(sh.contentBlocks))
	reasoningParts := make([]string, 0, len(sh.contentBlocks))
	toolCalls := make([]openAINonStreamToolCall, 0)

	for i := range sh.contentBlocks {
		blockType, _ := sh.contentBlocks[i]["type"].(string)
		switch blockType {
		case "thinking":
			if builder, ok := sh.thinkingBlockBuilders[i]; ok {
				if reasoning := builder.String(); reasoning != "" {
					reasoningParts = append(reasoningParts, reasoning)
					continue
				}
			}
			if reasoning, ok := sh.contentBlocks[i]["thinking"].(string); ok && reasoning != "" {
				reasoningParts = append(reasoningParts, reasoning)
			}
		case "text":
			if builder, ok := sh.textBlockBuilders[i]; ok {
				if text := builder.String(); text != "" {
					textParts = append(textParts, text)
					continue
				}
			}
			if text, ok := sh.contentBlocks[i]["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			call := openAINonStreamToolCall{
				Type: "function",
			}
			if id, ok := sh.contentBlocks[i]["id"].(string); ok {
				call.ID = id
			}
			if name, ok := sh.contentBlocks[i]["name"].(string); ok {
				call.Function.Name = name
			}
			switch input := sh.contentBlocks[i]["input"].(type) {
			case string:
				call.Function.Arguments = input
			case nil:
				call.Function.Arguments = "{}"
			default:
				raw, err := json.Marshal(input)
				if err != nil {
					call.Function.Arguments = "{}"
				} else {
					call.Function.Arguments = string(raw)
				}
			}
			toolCalls = append(toolCalls, call)
		}
	}

	content := strings.Join(textParts, "")
	if strings.TrimSpace(content) == "" && len(toolCalls) > 0 {
		content = ""
	}

	message := openAINonStreamMessage{
		Role:             "assistant",
		Content:          content,
		ReasoningContent: strings.Join(reasoningParts, ""),
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}

	return openAINonStreamResponse{
		ID:      sh.msgID,
		Object:  "chat.completion",
		Created: sh.startTime.Unix(),
		Model:   model,
		Choices: []openAINonStreamChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapStopReasonToOpenAIFinishReason(stopReason),
		}},
		Usage: openAINonStreamUsage{
			PromptTokens:     sh.inputTokens,
			CompletionTokens: sh.outputTokens,
			TotalTokens:      sh.inputTokens + sh.outputTokens,
		},
	}
}

func shortRequestTrace(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
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
				// Pre-compiled zero-allocation string
				fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"message\":\"Internal Server Error\"}}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			} else {
				apperrors.New("server_error", "Internal Server Error", http.StatusInternalServerError).WriteResponse(w)
			}
		}
	}()

	if r.Method != http.MethodPost {
		apperrors.New("invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed).WriteResponse(w)
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
				apperrors.New("invalid_request_error", "Request body too large", http.StatusRequestEntityTooLarge).WriteResponse(w)
				return
			}
		}
		apperrors.New("invalid_request_error", "Invalid request body", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		apperrors.New("invalid_request_error", "Invalid request body", http.StatusBadRequest).WriteResponse(w)
		return
	}
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		apperrors.New("permission_error", "API key is not allowed to use model "+strings.TrimSpace(req.Model), http.StatusForbidden).WriteResponse(w)
		return
	}
	responseFormat := adapter.DetectResponseFormat(r.URL.Path)

	// 初始化调试日志
	logger := debug.New(h.config.DebugEnabled, h.config.DebugLogSSE)
	defer logger.Close()
	verboseDiagnostics := logutil.VerboseDiagnosticsEnabled()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

	reqHash := h.computeRequestHash(r, bodyBytes)
	traceID := shortRequestTrace(reqHash)
	if verboseDiagnostics {
		slog.Debug("Request fingerprint", "trace_id", traceID, "hash", reqHash, "path", r.URL.Path, "content_length", len(bodyBytes))
	}

	// ...
	if ok, command := isCommandPrefixRequest(req); ok {
		if verboseDiagnostics {
			slog.Debug("Handling command prefix request", "command", command)
		}
		prefix := detectCommandPrefix(command)
		logger.LogEarlyExit("command_prefix", map[string]interface{}{
			"command": command,
			"prefix":  prefix,
		})
		writeCommandPrefixResponse(w, req, responseFormat, prefix, startTime, logger)
		return
	}

	if isTopicClassifierRequest(req) {
		if verboseDiagnostics {
			slog.Debug("Handling topic classifier request locally")
		}
		logger.LogEarlyExit("topic_classifier", map[string]interface{}{
			"mode": "local",
		})
		writeTopicClassifierResponse(w, req, responseFormat, startTime, logger)
		return
	}

	if isTitleGenerationRequest(req) {
		title := generateTopicTitle(extractUserText(req.Messages))
		if verboseDiagnostics {
			slog.Debug("Handling title generation request locally", "title", title)
		}
		logger.LogEarlyExit("title_generation", map[string]interface{}{
			"mode":  "local",
			"title": title,
		})
		writeTitleGenerationResponse(w, req, responseFormat, startTime, logger)
		return
	}

	cacheStrategy := h.config.CacheStrategy
	if cacheStrategy != "" && cacheStrategy != "none" {
		applyCacheStrategy(&req, cacheStrategy)
	}

	// Debug: log all headers
	if verboseDiagnostics {
		for k, v := range r.Header {
			slog.Debug("Incoming header V2 CHECK", "key", k, "value", v)
		}
	}

	// Context and Conversation Key
	conversationKey := conversationKeyForRequest(r, req)
	if verboseDiagnostics {
		slog.Debug("Request dispatch initialized", "trace_id", traceID, "path", r.URL.Path, "conversation_id", conversationKey, "model", req.Model, "stream", req.Stream)
	}

	forcedChannel := channelFromPath(r.URL.Path)
	validatedModel, err := h.validateModelAvailability(r.Context(), req.Model, forcedChannel)
	if err != nil {
		apperrors.New("invalid_request_error", err.Error(), http.StatusBadRequest).WriteResponse(w)
		return
	}
	targetChannel := strings.TrimSpace(forcedChannel)
	if targetChannel == "" && validatedModel != nil {
		targetChannel = strings.TrimSpace(validatedModel.Channel)
		if targetChannel == "" {
			targetChannel = ""
		}
	}
	effectiveWorkdir, prevWorkdir, workdirChanged := h.resolveWorkdir(r, req, conversationKey)
	if workdirChanged {
		slog.Info("工作目录变化，保留完整请求历史并新建上游会话", "prev", prevWorkdir, "next", effectiveWorkdir, "session", conversationKey)
		// 工作目录变化时清除上游会话ID，强制开启新对话
		if conversationKey != "" {
			h.sessionStore.DeleteSession(r.Context(), conversationKey)
		}
	}
	if isCurrentWorkdirRequest(req) {
		logger.LogEarlyExit("current_workdir", map[string]interface{}{
			"mode":    "local",
			"workdir": effectiveWorkdir,
			"path":    r.URL.Path,
		})
		writeCurrentWorkdirResponse(w, req, responseFormat, effectiveWorkdir, startTime, logger)
		return
	}
	if isSuggestionMode(req.Messages) {
		suggestion := buildLocalSuggestion(req.Messages)
		if verboseDiagnostics {
			slog.Debug("Handling suggestion mode request locally", "suggestion", suggestion)
		}
		logger.LogEarlyExit("suggestion_mode", map[string]interface{}{
			"mode":       "local",
			"suggestion": suggestion,
		})
		writeSuggestionModeResponse(w, req, responseFormat, startTime, logger)
		return
	}

	preSelectWarpRequest := strings.EqualFold(targetChannel, "warp")
	preSelectPuterRequest := strings.EqualFold(targetChannel, "puter")
	preSelectPassthroughRequest := preSelectWarpRequest || preSelectPuterRequest
	warpChatMode := preSelectWarpRequest && isWarpChatModel(req.Model)
	warpAgentMode := preSelectWarpRequest && isWarpAgentModel(req.Model)
	suggestionMode := isSuggestionMode(req.Messages)
	emptyOutputRecoveryPrompt := ""
	if preSelectWarpRequest {
		emptyOutputRecoveryPrompt = buildEmptyOutputRecoveryPrompt(req.Messages)
	}
	noThinking := suggestionMode || h.config.SuppressThinking
	gateNoTools := false
	toolGateReasons := make([]string, 0, 2)
	toolGateMessage := ""
	if suggestionMode {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "suggestion_mode")
		toolGateMessage = buildToolGateMessage(req.Messages, true)
	}
	if emptyOutputRecoveryPrompt != "" {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "empty_output_recovery")
		toolGateMessage = "Confirm the completed operation directly. Do not call tools or repeat the operation."
	}
	if lastUserIsToolResultFollowup(req.Messages) {
		if preSelectPassthroughRequest {
			if verboseDiagnostics {
				slog.Debug("tool_gate: keeping tools for passthrough tool_result follow-up", "warp", preSelectWarpRequest, "puter", preSelectPuterRequest)
			}
		} else {
			gateNoTools = true
			toolGateReasons = append(toolGateReasons, "tool_result_followup")
			toolGateMessage = buildToolGateMessage(req.Messages, suggestionMode)
			if verboseDiagnostics {
				slog.Debug("tool_gate: disabled tools for tool_result-only follow-up", "warp", preSelectWarpRequest)
			}
		}
	}
	effectiveTools := req.Tools
	if h.config.WarpDisableTools != nil && *h.config.WarpDisableTools {
		effectiveTools = nil
		if preSelectWarpRequest {
			gateNoTools = true
			toolGateReasons = append(toolGateReasons, "warp_tools_disabled")
			toolGateMessage = buildToolGateMessage(req.Messages, suggestionMode)
		}
	}
	// An API client that declares no tools cannot execute Warp's native tools.
	// Treat both an omitted tools field and tools:[] as an authoritative deny.
	if preSelectWarpRequest && len(req.Tools) == 0 {
		gateNoTools = true
		toolGateReasons = append(toolGateReasons, "client_no_tools")
		toolGateMessage = buildToolGateMessage(req.Messages, suggestionMode)
	}
	if warpChatMode {
		gateNoTools = true
		effectiveTools = nil
		toolGateReasons = append(toolGateReasons, "warp_chat_mode")
		toolGateMessage = warpChatToolGateMessage()
	}
	if gateNoTools {
		effectiveTools = nil
		if verboseDiagnostics {
			slog.Debug("tool_gate: disabled tools", "warp", preSelectWarpRequest, "reasons", toolGateReasons)
		}
	}
	requireWarpCloudAgent := preSelectWarpRequest && !warpChatMode && (warpAgentMode || warpRequestRequiresCloudAgent(req.Messages, effectiveTools))
	warpContinuationState := warpContinuation{}
	if preSelectWarpRequest {
		warpContinuationState, err = h.resolveWarpContinuation(r.Context(), conversationKey, req.Messages)
		if err != nil {
			apperrors.New("invalid_request_error", err.Error(), http.StatusConflict).WriteResponse(w)
			return
		}
	}
	chatSessionID := warpContinuationState.conversationID

	// 选择账号 (Initial Selection)
	failedAccountIDs := []int64{}
	failedAccountSet := make(map[int64]struct{})

	apiClient, currentAccount, err := h.selectAccountWithOptions(r.Context(), targetChannel, forcedChannel != "", failedAccountIDs, accountSelectionOptions{
		ModelID:               upstreamWarpModelID(req.Model),
		RequireWarpCloudAgent: requireWarpCloudAgent,
		PreferredAccountID:    warpContinuationState.accountID,
	})
	if err != nil {
		slog.Error("selectAccount failed", "error", err, "channel", targetChannel)
		logger.LogEarlyExit("select_account_failed", map[string]interface{}{
			"error":   err.Error(),
			"model":   req.Model,
			"channel": targetChannel,
		})
		apperrors.New("overloaded_error", err.Error(), http.StatusServiceUnavailable).WriteResponse(w)
		return
	}
	if verboseDiagnostics {
		slog.Debug("Checkpoint: selectAccount success")
	}

	isWarpRequest := preSelectWarpRequest
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "warp") {
		isWarpRequest = true
	}
	isPuterRequest := preSelectPuterRequest
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "puter") {
		isPuterRequest = true
	}
	isPassthroughRequest := isWarpRequest || isPuterRequest
	if isPassthroughRequest {
		channel := "warp"
		if isPuterRequest {
			channel = "puter"
		}
		// Passthrough channels do not trim history/tool results.
		if verboseDiagnostics {
			slog.Debug("Checkpoint: passthrough, skip context trimming", "channel", channel)
		}
	}
	if isPuterRequest {
		if sanitized, changed := sanitizeSystemItems(req.System, false, true, h.config); changed {
			req.System = sanitized
			if verboseDiagnostics {
				slog.Debug("puter: sanitized forwarded system items")
			}
		}
		if isDeepSeekPuterModel(req.Model) {
			restored, missing := h.restorePuterReasoning(r.Context(), req.Model, req.Messages)
			if verboseDiagnostics && (restored > 0 || missing > 0) {
				slog.Debug("puter reasoning replay prepared", "restored", restored, "fallback_required", missing)
			}
		}
	}
	if verboseDiagnostics {
		slog.Debug("Checkpoint: message processing done")
	}

	// 手动管理连接计数，账号切换时需要释放旧账号、获取新账号
	trackedAccountID := int64(0)
	trackedAccountID = h.acquireTrackedAccount(currentAccount)
	defer func() {
		h.releaseTrackedAccount(trackedAccountID)
	}()

	// 构建 prompt（V2 Markdown 格式）
	startBuild := time.Now()
	if verboseDiagnostics {
		slog.Debug("Starting prompt build...", "conversation_id", conversationKey)
	}
	// 映射模型（用于上游请求与提示一致）
	mappedModel := mapModel(req.Model)
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "warp") {
		mappedModel = upstreamWarpModelID(req.Model)
	} else if isPuterRequest {
		mappedModel = strings.TrimSpace(req.Model)
	}

	var builtPrompt string
	if isPuterRequest {
		builtPrompt = strings.TrimSpace(extractUserText(req.Messages))
		if builtPrompt == "" {
			builtPrompt = "puter request"
		}
	} else {
		builtPrompt = warp.PreviewUserQuery("", req.Messages, req.System, chatSessionID)
		if emptyOutputRecoveryPrompt != "" {
			builtPrompt = emptyOutputRecoveryPrompt
		}
		if strings.TrimSpace(builtPrompt) == "" && !(isWarpRequest && len(latestToolResultIDs(req.Messages)) > 0) {
			builtPrompt = "warp request"
		}
	}
	buildDuration := time.Since(startBuild)
	if verboseDiagnostics {
		slog.Debug("Prompt build completed", "duration", buildDuration)
		slog.Debug("[Performance] BuildPromptAndHistory", "duration", buildDuration)
	}

	if verboseDiagnostics {
		slog.Debug("Model mapping", "original", req.Model, "mapped", mappedModel)
	}

	isStream := req.Stream

	if isStream {
		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		if _, ok := w.(http.Flusher); !ok {
			apperrors.New("api_error", "Streaming not supported by underlying connection", http.StatusInternalServerError).WriteResponse(w)
			return
		}
		streamingStarted = true
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// 状态管理
	// msgID is now managed by streamHandler

	upstreamMessages := append([]prompt.Message(nil), req.Messages...)

	if gateNoTools {
		builtPrompt = injectToolGate(builtPrompt, toolGateMessage)
	}

	// 2. 记录转换后的 prompt
	if verboseDiagnostics {
		slog.Debug("Checkpoint: LogConvertedPrompt")
	}
	logger.LogConvertedPrompt(builtPrompt)

	breakdown := estimateInputTokenBreakdown(builtPrompt, effectiveTools)
	breakdownProfile := "warp"
	if isPuterRequest {
		breakdownProfile = "puter"
	}
	if isWarpRequest {
		if warpBD, profile, err := estimateWarpInputTokenBreakdown(builtPrompt, mappedModel, upstreamMessages, req.System, effectiveTools, gateNoTools, chatSessionID); err == nil {
			breakdown = warpBD
			breakdownProfile = profile
		} else {
			slog.Warn("Warp token estimation fallback to generic breakdown", "error", err)
		}
	}
	if verboseDiagnostics {
		slog.Debug(
			"Input token breakdown (estimated)",
			"prompt_profile", breakdownProfile,
			"base_prompt_tokens", breakdown.BasePromptTokens,
			"system_context_tokens", breakdown.SystemContextTokens,
			"history_tokens", breakdown.HistoryTokens,
			"tools_tokens", breakdown.ToolsTokens,
			"estimated_total_input_tokens", breakdown.Total,
		)
	}
	logger.LogInputTokenBreakdown(
		breakdownProfile,
		breakdown.BasePromptTokens,
		breakdown.SystemContextTokens,
		breakdown.HistoryTokens,
		breakdown.ToolsTokens,
		breakdown.Total,
	)

	// Token 计数（用于前置 usage 展示）
	inputTokens := breakdown.Total
	if inputTokens <= 0 {
		inputTokens = h.estimateInputTokens(r.Context(), req.Model, builtPrompt)
	}

	if h.config.EnableTokenCache && h.promptCache != nil {
		sysText := ""
		if len(req.System) > 0 {
			if sysBytes, err := json.Marshal(req.System); err == nil {
				sysText = string(sysBytes)
			}
		}
		toolsText := ""
		if len(effectiveTools) > 0 {
			if toolsBytes, err := json.Marshal(effectiveTools); err == nil {
				toolsText = string(toolsBytes)
			}
		}

		cacheReadTokens, _ := h.promptCache.CheckPromptCache(
			h.config.TokenCacheStrategy,
			breakdown.SystemContextTokens,
			breakdown.ToolsTokens,
			sysText,
			toolsText,
		)
		// Subtract cacheReadTokens from the base inputTokens
		// if simulating prompt caching billing behavior
		if inputTokens >= cacheReadTokens {
			inputTokens -= cacheReadTokens
		}
	}

	sh := newStreamHandler(
		h.config, w, logger, noThinking, isStream, responseFormat, effectiveWorkdir,
	)
	allowedToolNames := []string(nil)
	allowedToolNames = validationAllowedToolNames(effectiveTools, req.Tools, false)
	sh.setAllowedToolNames(allowedToolNames)
	if preSelectWarpRequest {
		sh.setStrictToolAllowlist(true)
		sh.setSurfaceToolRejects(true)
	}
	if len(req.Tools) > 0 {
		sh.setClientTools(req.Tools)
	} else if len(effectiveTools) > 0 {
		sh.setClientTools(effectiveTools)
	}
	sh.setDisallowToolCalls(gateNoTools)
	sh.setEmptyOutputFallback(successfulFileMutationToolResultFallback(upstreamMessages))
	sh.setUsageTokens(inputTokens, -1) // Correctly initialize input tokens
	activeWarpConversationID := chatSessionID
	// Capture the server-issued Warp conversation and bind it to both an
	// explicit client session (when present) and every emitted tool call.
	sh.onConversationID = func(id string) {
		activeWarpConversationID = strings.TrimSpace(id)
		if conversationKey != "" {
			h.sessionStore.SetConvID(r.Context(), conversationKey, activeWarpConversationID)
			if currentAccount != nil {
				h.sessionStore.SetAccountID(r.Context(), conversationKey, currentAccount.ID)
			}
			h.sessionStore.Touch(r.Context(), conversationKey)
		}
		if verboseDiagnostics {
			slog.Debug("Warp conversationID captured", "key", conversationKey, "id", activeWarpConversationID)
		}
	}
	sh.onToolCall = func(id, name, input, upstreamType string) {
		if isPuterRequest && isDeepSeekPuterModel(mappedModel) {
			if reasoning := sh.currentReasoningText(); reasoning != "" {
				if err := h.savePuterReasoningForTool(r.Context(), mappedModel, id, reasoning); err != nil {
					slog.Warn("failed to save puter reasoning replay", "tool_call_id", id, "error", err)
				}
			}
		}
		if !isWarpRequest || activeWarpConversationID == "" {
			return
		}
		accountID := int64(0)
		if currentAccount != nil {
			accountID = currentAccount.ID
		}
		// Tool continuation is only safe when the caller supplied a stable
		// conversation namespace. Never create a globally addressable binding.
		if conversationKey == "" {
			return
		}
		h.sessionStore.SetWarpToolBinding(r.Context(), conversationKey, id, WarpToolBinding{
			ConversationID: activeWarpConversationID,
			AccountID:      accountID,
			ToolType:       upstreamType,
			ToolName:       name,
			ToolInput:      warpBindingInput(upstreamType, input),
		})
	}
	sh.onModelConfigRefresh = func() {
		if isWarpRequest {
			h.refreshWarpModelConfigAsync(currentAccount)
		}
	}
	defer sh.release()

	sh.writeSSEMessageStart(req.Model, inputTokens, 0)

	if verboseDiagnostics {
		slog.Debug("New request received")
	}

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
		if chatSessionID == "" && !isWarpRequest {
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

		warpFeatureConfig := h.resolveWarpFeatureConfig(r.Context(), currentAccount, mappedModel)
		upstreamReq := upstream.UpstreamRequest{
			Prompt:               builtPrompt,
			Workdir:              effectiveWorkdir,
			Model:                mappedModel,
			Stream:               req.Stream,
			Messages:             payloadMessages,
			System:               payloadSystem,
			Tools:                effectiveTools,
			NoTools:              gateNoTools,
			ChatSessionID:        chatSessionID,
			ProjectID:            "",
			WarpCliAgentModel:    warpFeatureConfig.CliAgentModel,
			WarpComputerUseModel: warpFeatureConfig.ComputerUseAgentModel,
			WarpToolContexts:     warpContinuationState.toolContexts,
		}
		primaryHandler := sh.handleMessage
		var attempt int
		for {
			sh.resetRoundState()
			var err error
			upstreamReq.Attempt = attempt + 1
			accountID := int64(0)
			accountType := ""
			accountName := ""
			if currentAccount != nil {
				accountID = currentAccount.ID
				accountType = currentAccount.AccountType
				accountName = currentAccount.Name
			}
			if verboseDiagnostics {
				slog.Debug(
					"Calling upstream client",
					"trace_id", traceID,
					"attempt", upstreamReq.Attempt,
					"max_attempts", maxRetries+1,
					"channel", targetChannel,
					"model", mappedModel,
					"conversation_id", conversationKey,
					"chat_session_id", chatSessionID,
					"account_id", accountID,
					"account_type", accountType,
					"account_name", accountName,
				)
			}

			if verboseDiagnostics {
				slog.Debug("Using SendRequestWithPayload")
			}
			err = apiClient.SendRequestWithPayload(r.Context(), upstreamReq, primaryHandler, logger)
			if verboseDiagnostics {
				slog.Debug("Upstream client returned", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)
			}

			if err == nil {
				sh.forceFinishIfMissing()
				if verboseDiagnostics {
					slog.Debug("Upstream attempt completed", "trace_id", traceID, "attempt", upstreamReq.Attempt)
				}
				break
			}
			errStr := err.Error()
			errClass := apperrors.ClassifyUpstreamError(errStr)
			warpCloudAgentForbidden := isWarpCloudAgentForbiddenError(errStr)
			warpRequestStarted := isWarpRequest && warp.RequestIDFromError(err) != ""
			warpRefundConfirmed := false
			if isWarpRequest {
				warpRefundConfirmed = h.refundWarpCredits(apiClient, err, errClass.Category)
			}
			if sh.hasAnyOutput() {
				slog.Warn("Upstream failed after partial output, skip retry to avoid duplicated token billing", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err)
				if !sh.hasVisibleOutput() {
					sh.InjectErrorText("Reporting failure after hidden upstream output", "Upstream request failed after generating reasoning. Automatic retry was suppressed to avoid duplicate token billing.")
				}
				sh.finishResponse("end_turn")
				return
			}
			if warpRequestStarted && shouldRefundWarpCredits(errClass.Category) && !warpRefundConfirmed {
				slog.Warn("Warp retry suppressed because the started request may have been billed and refund was not confirmed", "trace_id", traceID, "attempt", upstreamReq.Attempt, "category", errClass.Category, "request_id", warp.RequestIDFromError(err))
				if errClass.Category != "canceled" {
					sh.InjectErrorText("Suppressing potentially billed Warp retry", fmt.Sprintf("Request failed: %s", strings.TrimSpace(errStr)))
				}
				sh.finishResponse("end_turn")
				return
			}

			// Check for non-retriable errors
			slog.Error("Request error", "trace_id", traceID, "attempt", upstreamReq.Attempt, "error", err, "category", errClass.Category, "retryable", errClass.Retryable)
			// 标记账号状态（auth 类错误始终标记，无论是否可重试）
			if currentAccount != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
				if status := apperrors.ClassifyAccountStatus(errStr); status != "" {
					// Mark status if it's auth-related OR a quota/rate-limit style cooldown.
					if !errClass.Retryable || errClass.Category == "auth" || errClass.Category == "auth_blocked" || status == "403" || status == "429" || status == "402" {
						skipAccountStatusMark := (isWarpRequest && status == "403" && warpCloudAgentForbidden) ||
							(strings.EqualFold(strings.TrimSpace(targetChannel), "puter") && status == "429" && isPuterModelScopedRateLimit(errStr))
						if skipAccountStatusMark {
							if verboseDiagnostics {
								slog.Debug("跳过账号全局 403 标记: Warp cloud agent 能力不足", "account_id", currentAccount.ID, "category", errClass.Category)
							}
						} else if verboseDiagnostics {
							slog.Debug("标记账号状态", "account_id", currentAccount.ID, "status", status, "category", errClass.Category)
						}
						if !skipAccountStatusMark {
							if isWarpRequest && errClass.Category == "rate_limit" && isWarpQuotaExhaustedError(errStr) {
								markWarpQuotaExhausted(r.Context(), h.loadBalancer.Store, currentAccount)
							} else {
								h.loadBalancer.MarkAccountStatus(r.Context(), currentAccount, status)
							}
						}
					}
				}
			}

			if !errClass.Retryable {
				slog.Error("Aborting retries for non-retriable error", "error", err, "category", errClass.Category)
				if errClass.Category == "auth_blocked" || errClass.Category == "auth" {
					sh.InjectAuthError(errStr)
				} else if errClass.Category != "canceled" {
					sh.InjectErrorText("Injecting upstream error to client", fmt.Sprintf("Request failed: %s", strings.TrimSpace(errStr)))
				}
				if errClass.Category == "canceled" {
					sh.finishResponse("end_turn")
					return
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
				if errClass.Category == "auth" || errClass.Category == "auth_blocked" {
					sh.InjectAuthError(errStr)
				} else {
					sh.InjectErrorText("Injecting retry exhausted error to client", fmt.Sprintf("Request failed: retries exhausted. Last error: %s", errStr))
				}
				sh.finishResponse("end_turn")
				return
			}
			retriesRemaining--
			slog.Warn(
				"Retrying upstream request without prior output",
				"trace_id", traceID,
				"attempt", upstreamReq.Attempt,
				"category", errClass.Category,
				"switch_account", errClass.SwitchAccount,
				"retries_remaining", retriesRemaining,
			)
			if errClass.SwitchAccount && currentAccount != nil && h.loadBalancer != nil {
				if isWarpRequest && warpCloudAgentForbidden {
					requireWarpCloudAgent = true
				}
				prevClient := apiClient
				prevAccount := currentAccount
				if _, ok := failedAccountSet[currentAccount.ID]; !ok {
					failedAccountSet[currentAccount.ID] = struct{}{}
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
				}
				slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "unsuccessful_attempts", len(failedAccountIDs))

				// 释放旧账号的连接计数
				if trackedAccountID != 0 {
					h.releaseTrackedAccount(trackedAccountID)
					trackedAccountID = 0
				}

				nextClient, nextAccount, retryErr := h.selectAccountWithOptions(r.Context(), targetChannel, forcedChannel != "", failedAccountIDs, accountSelectionOptions{
					ModelID:               upstreamReq.Model,
					RequireWarpCloudAgent: requireWarpCloudAgent,
					PreferredAccountID:    warpContinuationState.accountID,
				})
				if retryErr == nil {
					apiClient = nextClient
					currentAccount = nextAccount
					if currentAccount != nil {
						trackedAccountID = h.acquireTrackedAccount(currentAccount)
						warpFeatureConfig = h.resolveWarpFeatureConfig(r.Context(), currentAccount, upstreamReq.Model)
						upstreamReq.WarpCliAgentModel = warpFeatureConfig.CliAgentModel
						upstreamReq.WarpComputerUseModel = warpFeatureConfig.ComputerUseAgentModel
						if verboseDiagnostics {
							slog.Debug("Switched to account", "account", currentAccount.Name)
						}
					} else {
						warpFeatureConfig = warp.AccountFeatureConfig{}
						upstreamReq.WarpCliAgentModel = ""
						upstreamReq.WarpComputerUseModel = ""
						if verboseDiagnostics {
							slog.Debug("Switched to default upstream config")
						}
					}
				} else {
					if shouldRetryCurrentAccountForRequest(errClass.Category, targetChannel, errStr) && prevAccount != nil {
						apiClient = prevClient
						currentAccount = prevAccount
						trackedAccountID = h.acquireTrackedAccount(currentAccount)
						warpFeatureConfig = h.resolveWarpFeatureConfig(r.Context(), currentAccount, upstreamReq.Model)
						upstreamReq.WarpCliAgentModel = warpFeatureConfig.CliAgentModel
						upstreamReq.WarpComputerUseModel = warpFeatureConfig.ComputerUseAgentModel
						slog.Warn(
							"No alternate accounts available; retrying current account",
							"trace_id", traceID,
							"attempt", upstreamReq.Attempt,
							"account_id", currentAccount.ID,
							"category", errClass.Category,
							"retry_error", retryErr,
						)
					} else {
						slog.Error("No more accounts available", "error", retryErr)
						sh.InjectNoAvailableAccountError(errStr, retryErr)
						sh.finishResponse("end_turn")
						return
					}
				}
			}
			if retryDelay > 0 {
				delay := computeRetryDelay(retryDelay, attempt+1, errClass.Category)
				if delay > 0 && !util.SleepWithContext(r.Context(), delay) {
					sh.finishResponse("end_turn")
					return
				}
			}
			attempt++
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
		if sh.contentBlocks == nil {
			sh.contentBlocks = make([]map[string]interface{}, 0)
		}

		var response interface{}
		if responseFormat == adapter.FormatOpenAI {
			response = buildOpenAINonStreamResponse(sh, req.Model, stopReason)
		} else {
			anthropicResponse := map[string]interface{}{
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
			response = anthropicResponse
		}

		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("Failed to write JSON response", "error", err)
		}

	}

	// Sync state and update stats using helpers
	h.syncWarpState(currentAccount, apiClient)
	h.updateAccountStats(currentAccount, sh.inputTokens, sh.outputTokens)

	// Audit log
	if h.auditLogger != nil {
		accountID := int64(0)
		channel := forcedChannel
		if currentAccount != nil {
			accountID = currentAccount.ID
			if channel == "" {
				channel = currentAccount.AccountType
			}
		}
		status := "success"
		if sh.finalStopReason == "" && !sh.hasReturn {
			status = "error"
		}
		h.auditLogger.Log(r.Context(), audit.Event{
			Action:    "chat_request",
			AccountID: accountID,
			Model:     req.Model,
			Channel:   channel,
			ClientIP:  r.RemoteAddr,
			UserAgent: r.UserAgent(),
			Duration:  time.Since(startTime).Milliseconds(),
			Status:    status,
			Metadata: map[string]interface{}{
				"input_tokens":  sh.inputTokens,
				"output_tokens": sh.outputTokens,
				"stream":        isStream,
			},
		})
	}
}

func randomSessionID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based if crypto/rand fails (unlikely)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
