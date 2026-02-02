package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/orchids"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/internal/tokencache"
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

	sessionWorkdirs sync.Map // Map conversationKey -> string (workdir)
}

type UpstreamClient interface {
	SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(orchids.SSEMessage), logger *debug.Logger) error
}

type UpstreamPayloadClient interface {
	SendRequestWithPayload(ctx context.Context, req orchids.UpstreamRequest, onMessage func(orchids.SSEMessage), logger *debug.Logger) error
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
const maxRequestBytes = 32 * 1024 * 1024
const maxToolFollowups = 1

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		config:       cfg,
		client:       orchids.New(cfg),
		loadBalancer: lb,
		summaryLog:   cfg.SummaryCacheLog,
	}
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

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	if r.Method != http.MethodPost {
		h.writeErrorResponse(w, "invalid_request_error", "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.writeErrorResponse(w, "invalid_request_error", "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.writeErrorResponse(w, "invalid_request_error", "Invalid request body", http.StatusBadRequest)
		return
	}

	// 初始化调试日志
	logger := debug.New(h.config.DebugEnabled, h.config.DebugLogSSE)
	defer logger.Close()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

	if ok, command := isCommandPrefixRequest(req); ok {
		prefix := detectCommandPrefix(command)
		writeCommandPrefixResponse(w, req, prefix, startTime, logger)
		return
	}

	cacheStrategy := h.config.CacheStrategy
	if cacheStrategy != "" && cacheStrategy != "none" {
		applyCacheStrategy(&req, cacheStrategy)
	}

	// Debug: log all headers
	for k, v := range r.Header {
		slog.Debug("Incoming header", "key", k, "value", v)
	}

	forcedChannel := channelFromPath(r.URL.Path)

	// Check for dynamic workdir header EARLY
	dynamicWorkdir := r.Header.Get("X-Orchids-Workdir")
	if dynamicWorkdir == "" {
		dynamicWorkdir = r.Header.Get("X-Project-Root") // Try alternative
	}
	if dynamicWorkdir == "" {
		dynamicWorkdir = r.Header.Get("X-Working-Dir") // Try another alternative
	}

	// FALLBACK: Check system prompt for <env>Working directory: ...</env>
	if dynamicWorkdir == "" {
		dynamicWorkdir = extractWorkdirFromSystem(req.System)
		if dynamicWorkdir != "" {
			slog.Info("Using workdir from system prompt env block", "workdir", dynamicWorkdir)
		}
	}

	conversationKey := conversationKeyForRequest(r, req)

	// FINAL FALLBACK: Check session persistence
	if dynamicWorkdir == "" {
		if val, ok := h.sessionWorkdirs.Load(conversationKey); ok {
			dynamicWorkdir = val.(string)
			slog.Info("Recovered workdir from session", "workdir", dynamicWorkdir, "session", conversationKey)
		}
	}

	// Persist for future turns in this session
	if dynamicWorkdir != "" {
		h.sessionWorkdirs.Store(conversationKey, dynamicWorkdir)
	}

	effectiveWorkdir := ""
	if dynamicWorkdir != "" {
		effectiveWorkdir = dynamicWorkdir
		slog.Info("Using dynamic workdir", "workdir", dynamicWorkdir)
	}

	// 选择账号
	var apiClient UpstreamClient
	var currentAccount *store.Account
	var failedAccountIDs []int64
	failedAccountSet := make(map[int64]struct{})

	selectAccount := func() error {
		if h.loadBalancer != nil {
			targetChannel := forcedChannel
			if targetChannel == "" {
				targetChannel = h.loadBalancer.GetModelChannel(r.Context(), req.Model)
			}
			if targetChannel != "" {
				slog.Info("Model recognition", "model", req.Model, "channel", targetChannel)
			}
			account, err := h.loadBalancer.GetNextAccountExcludingByChannel(r.Context(), failedAccountIDs, targetChannel)
			if err != nil {
				if h.client != nil {
					apiClient = h.client
					currentAccount = nil
					slog.Info("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
					return nil
				}
				return err
			}
			if strings.EqualFold(account.AccountType, "warp") {
				apiClient = warp.NewFromAccount(account, h.config)
			} else {
				apiClient = orchids.NewFromAccount(account, h.config)
			}

			currentAccount = account
			return nil
		} else if h.client != nil {
			apiClient = h.client

			currentAccount = nil
			return nil
		}
		return errors.New("no client configured")
	}

	if err := selectAccount(); err != nil {
		h.writeErrorResponse(w, "overloaded_error", err.Error(), http.StatusServiceUnavailable)
		return
	}

	if currentAccount != nil && h.loadBalancer != nil {
		h.loadBalancer.AcquireConnection(currentAccount.ID)
		defer h.loadBalancer.ReleaseConnection(currentAccount.ID)
	}

	var hitsBefore, missesBefore uint64
	if h.summaryStats != nil && h.summaryLog {
		hitsBefore, missesBefore = h.summaryStats.Snapshot()
	}

	userText := extractUserText(req.Messages)
	planMode := isPlanMode(req.Messages)
	suggestionMode := isSuggestionMode(req.Messages)
	gateNoTools := false
	suppressThinking := false
	if suggestionMode {
		gateNoTools = true
		suppressThinking = true
	}
	effectiveTools := req.Tools
	if h.config.WarpDisableTools != nil && *h.config.WarpDisableTools {
		effectiveTools = nil
	}
	if gateNoTools {
		effectiveTools = nil
		slog.Debug("tool_gate: disabled tools for short non-code request")
	}
	toolCallMode := strings.ToLower(strings.TrimSpace(h.config.ToolCallMode))
	if toolCallMode == "" {
		toolCallMode = "proxy"
	}
	if planMode {
		toolCallMode = "proxy"
	}
	if toolCallMode == "auto" || toolCallMode == "internal" {
		effectiveTools = filterSupportedTools(effectiveTools)
	}

	if toolCallMode == "internal" && req.Stream {
		req.Stream = false
	}

	// 构建 prompt（V2 Markdown 格式）
	startBuild := time.Now()

	opts := prompt.PromptOptions{
		Context:          r.Context(),
		ConversationID:   conversationKey,
		MaxTokens:        h.config.ContextMaxTokens,
		SummaryMaxTokens: h.config.ContextSummaryMaxTokens,
		KeepTurns:        h.config.ContextKeepTurns,
		SummaryCache:     h.summaryCache,
		ProjectRoot:      effectiveWorkdir,
	}

	if c, ok := apiClient.(*orchids.Client); ok {
		opts.ProjectContext = c.GetProjectSummary()
	}

	slog.Debug("Starting prompt build...", "conversation_id", conversationKey)
	builtPrompt := prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
		Model:    req.Model,
		Messages: req.Messages,
		System:   req.System,
		Tools:    effectiveTools,
		Stream:   req.Stream,
	}, opts)
	slog.Debug("Prompt build completed", "duration", time.Since(startBuild))
	if h.config.DebugEnabled {
		slog.Info("[Performance] BuildPromptV2WithOptions", "duration", time.Since(startBuild))
		if opts.ProjectContext != "" {
			slog.Debug("Project context injected", "context", opts.ProjectContext)
		}
	}

	// ... rest of code

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

		if _, ok := w.(http.Flusher); !ok {
			h.writeErrorResponse(w, "api_error", "Streaming not supported by underlying connection", http.StatusInternalServerError)
			return
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// 状态管理
	// msgID is now managed by streamHandler

	allowedTools := map[string]string{}
	var allowedIndex []toolNameInfo
	for _, t := range effectiveTools {
		if tm, ok := t.(map[string]interface{}); ok {
			if name, ok := tm["name"].(string); ok {
				name = strings.TrimSpace(name)
				if name != "" {
					allowedTools[strings.ToLower(name)] = name
				}
			}
		}
	}
	allowedIndex = buildToolNameIndex(effectiveTools, allowedTools)
	hasToolList := len(allowedTools) > 0
	resolveToolName := func(name string) (string, bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", false
		}
		if !hasToolList {
			return name, true
		}
		if resolved, ok := allowedTools[strings.ToLower(name)]; ok {
			return resolved, true
		}
		return "", false
	}

	var preflightResults []safeToolResult

	chatHistory := []interface{}{}
	upstreamMessages := append([]prompt.Message(nil), req.Messages...)
	allowBashName := ""
	shouldLocalFallback := false
	if name, ok := resolveToolName("bash"); ok {
		allowBashName = name
	}
	if (toolCallMode == "internal" || toolCallMode == "auto") && allowBashName != "" && shouldPreflightTools(userText) {
		preflight := []string{
			"pwd",
			"find . -maxdepth 2 -not -path '*/.*'",
			"ls -la",
		}
		for i, cmd := range preflight {
			call := toolCall{
				id:    fmt.Sprintf("internal_tool_%d", i+1),
				name:  allowBashName,
				input: fmt.Sprintf(`{"command":%q,"description":"internal preflight"}`, cmd),
			}
			result := executeSafeTool(call)
			preflightResults = append(preflightResults, result)
			chatHistory = append(chatHistory, map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{
						"type":  "tool_use",
						"id":    result.call.id,
						"name":  result.call.name,
						"input": result.input,
					},
				},
			})
			chatHistory = append(chatHistory, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{
						"type":        "tool_result",
						"tool_use_id": result.call.id,
						"content":     result.output,
						"is_error":    result.isError,
					},
				},
			})
		}
		shouldLocalFallback = len(preflightResults) > 0
	}

	localContext := formatLocalToolResults(preflightResults)
	if localContext != "" {
		builtPrompt = injectLocalContext(builtPrompt, localContext)
	}
	if gateNoTools {
		builtPrompt = injectToolGate(builtPrompt, "This is a short, non-code request. Do NOT call tools or perform any file operations. Answer directly.")
	}

	// 2. 记录转换后的 prompt
	logger.LogConvertedPrompt(builtPrompt)

	// Token 计数
	inputTokens := h.estimateInputTokens(r.Context(), req.Model, builtPrompt)

	// Detect Response Format (Anthropic vs OpenAI)
	responseFormat := "anthropic"
	if strings.Contains(r.URL.Path, "/chat/completions") {
		responseFormat = "openai"
	}

	sh := newStreamHandler(
		h.config, w, logger, toolCallMode, suppressThinking, allowedTools, allowedIndex, preflightResults, shouldLocalFallback, isStream, responseFormat,
	)
	sh.setUsageTokens(inputTokens, -1) // Correctly initialize input tokens
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
		ticker := time.NewTicker(keepAliveInterval)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sh.mu.Lock()
					if sh.hasReturn {
						sh.mu.Unlock()
						return
					}
					fmt.Fprint(w, ": keepalive\n\n")
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					sh.mu.Unlock()
				case <-keepAliveStop:
					return
				case <-r.Context().Done():
					return
				}
			}
		}()
	}

	run := func() {
		turnCount := 1
		followupCount := 0
		chatSessionID := "chat_" + randomSessionID()

		for {
			slog.Debug("Starting turn", "turn", turnCount, "mode", toolCallMode)
			sh.internalNeedsFollowup = false // Reset per retry/turn
			sh.internalToolResults = nil
			maxRetries := h.config.MaxRetries
			if maxRetries < 0 {
				maxRetries = 0
			}
			retryDelay := time.Duration(h.config.RetryDelay) * time.Millisecond
			retriesRemaining := maxRetries

			upstreamReq := orchids.UpstreamRequest{
				Prompt:        builtPrompt,
				ChatHistory:   chatHistory,
				Workdir:       dynamicWorkdir,
				Model:         mappedModel,
				Messages:      upstreamMessages,
				System:        []prompt.SystemItem(req.System),
				Tools:         effectiveTools,
				NoTools:       gateNoTools,
				NoThinking:    suggestionMode,
				ChatSessionID: chatSessionID,
			}
			for {
				sh.resetRoundState() // Ensure fresh state indicators for each attempt
				var err error
				if sender, ok := apiClient.(UpstreamPayloadClient); ok {
					err = sender.SendRequestWithPayload(r.Context(), upstreamReq, sh.handleMessage, logger)
				} else {
					err = apiClient.SendRequest(r.Context(), builtPrompt, chatHistory, mappedModel, sh.handleMessage, logger)
				}

				if err == nil {
					break
				}

				slog.Error("Request error", "error", err)

				// Check for non-retriable errors
				errStr := err.Error()
				isAuthError := strings.Contains(errStr, "401") || strings.Contains(errStr, "403") || strings.Contains(errStr, "404") || strings.Contains(errStr, "Signed out") || strings.Contains(errStr, "signed_out")
				if isAuthError || strings.Contains(errStr, "Input is too long") || strings.Contains(errStr, "400") {
					slog.Error("Aborting retries for non-retriable error", "error", err)

					// Disable the account if it's an authentication, forbidden, or not found error
					if (strings.Contains(errStr, "403") || strings.Contains(errStr, "401") || strings.Contains(errStr, "404")) && currentAccount != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
						slog.Warn("Disabling account due to critical error", "account_id", currentAccount.ID, "account_name", currentAccount.Name, "error", err)
						currentAccount.Enabled = false
						if updateErr := h.loadBalancer.Store.UpdateAccount(context.Background(), currentAccount); updateErr != nil {
							slog.Error("Failed to disable account in store", "account_id", currentAccount.ID, "error", updateErr)
						}
					}

					// Inject error message to client for better visibility
					if isAuthError {
						errorMsg := fmt.Sprintf("Warp Request Failed: %s. Please check your account status.", errStr)
						if strings.Contains(errStr, "401") {
							errorMsg = "Authentication Error: Session expired (401). Please update your account credentials."
						} else if strings.Contains(errStr, "403") {
							errorMsg = "Access Forbidden (403): Your account might be flagged or blocked by Warp's firewall. Try re-enabling it in the Admin UI."
						}
						idx := sh.ensureBlock("text")

						// For stream, send delta immediately
						if sh.isStream {
							deltaMap := map[string]interface{}{
								"type":  "content_block_delta",
								"index": idx,
								"delta": map[string]interface{}{
									"type": "text_delta",
									"text": errorMsg,
								},
							}
							deltaData, _ := json.Marshal(deltaMap)
							sh.writeSSE("content_block_delta", string(deltaData))
						} else {
							// For non-stream, ensureBlock has initialized the builder, we append to it
							if builder, ok := sh.textBlockBuilders[idx]; ok {
								builder.WriteString(errorMsg)
							}
						}
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
					sh.finishResponse("end_turn")
					return
				}
				retriesRemaining--
				if currentAccount != nil && h.loadBalancer != nil {
					if _, ok := failedAccountSet[currentAccount.ID]; !ok {
						failedAccountSet[currentAccount.ID] = struct{}{}
						failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
					}
					slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "failed_count", len(failedAccountIDs))
					if retryErr := selectAccount(); retryErr == nil {
						if currentAccount != nil {
							slog.Debug("Switched to account", "account", currentAccount.Name)
						} else {
							slog.Debug("Switched to default upstream config")
						}
					} else {
						slog.Error("No more accounts available", "error", retryErr)
						sh.finishResponse("end_turn")
						return
					}
				}
				if retryDelay > 0 {
					if !util.SleepWithContext(r.Context(), retryDelay) {
						sh.finishResponse("end_turn")
						return
					}
				}
			}
			if ((toolCallMode == "internal" || toolCallMode == "auto") && sh.internalNeedsFollowup) || (sh.internalNeedsFollowup && len(sh.internalToolResults) > 0) {
				if toolCallMode == "internal" || toolCallMode == "auto" {
					if followupCount >= maxToolFollowups {
						slog.Warn("Tool follow-up limit reached", "turn", turnCount, "max_followups", maxToolFollowups)
						sh.finishResponse("end_turn")
						return
					}
					followupCount++
				}
				slog.Debug("Turn completed, follow-up required", "turn", turnCount)
				turnCount++
				if len(sh.internalToolResults) > 0 {
					var assistantBlocks []prompt.ContentBlock
					var userBlocks []prompt.ContentBlock
					var assistantHistoryBlocks []map[string]interface{}
					var userHistoryBlocks []map[string]interface{}

					// 1. Add text/thinking blocks from the current turn
					for _, block := range sh.contentBlocks {
						blockType, _ := block["type"].(string)
						if blockType == "text" {
							text, _ := block["text"].(string)
							if text != "" {
								assistantBlocks = append(assistantBlocks, prompt.ContentBlock{
									Type: "text",
									Text: text,
								})
								assistantHistoryBlocks = append(assistantHistoryBlocks, map[string]interface{}{
									"type": "text",
									"text": text,
								})
							}
						} else if blockType == "thinking" {
							// Check if we need to include thinking blocks in history
							// Usually we don't for Anthropic API unless explicitly requested,
							// but for "chat history" it might be needed if the model relies on it.
							// For now, let's include it if allowed.
							thinking, _ := block["thinking"].(string)
							if thinking != "" {
								assistantBlocks = append(assistantBlocks, prompt.ContentBlock{
									Type:     "thinking",
									Thinking: thinking,
								})
								assistantHistoryBlocks = append(assistantHistoryBlocks, map[string]interface{}{
									"type":     "thinking",
									"thinking": thinking,
								})
							}
						}
					}

					// 2. Add tool use and results
					for _, result := range sh.internalToolResults {
						assistantBlocks = append(assistantBlocks, prompt.ContentBlock{
							Type:  "tool_use",
							ID:    result.call.id,
							Name:  result.call.name,
							Input: result.input,
						})
						userBlocks = append(userBlocks, prompt.ContentBlock{
							Type:      "tool_result",
							ToolUseID: result.call.id,
							Content:   result.output,
							IsError:   result.isError,
						})
						assistantHistoryBlocks = append(assistantHistoryBlocks, map[string]interface{}{
							"type":  "tool_use",
							"id":    result.call.id,
							"name":  result.call.name,
							"input": result.input,
						})
						userHistoryBlocks = append(userHistoryBlocks, map[string]interface{}{
							"type":        "tool_result",
							"tool_use_id": result.call.id,
							"content":     result.output,
							"is_error":    result.isError,
						})
					}

					upstreamMessages = append(upstreamMessages,
						prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: assistantBlocks}},
						prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: userBlocks}},
					)
					chatHistory = append(chatHistory,
						map[string]interface{}{"role": "assistant", "content": assistantHistoryBlocks},
						map[string]interface{}{"role": "user", "content": userHistoryBlocks},
					)

					// Rebuild prompt for next round to include updated history
					builtPrompt = prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
						Model:    req.Model,
						Messages: upstreamMessages,
						System:   req.System,
						Tools:    effectiveTools,
						Stream:   req.Stream,
					}, opts)
					inputTokens = h.estimateInputTokens(r.Context(), req.Model, builtPrompt)
					sh.setUsageTokens(inputTokens, -1)
					logger.LogConvertedPrompt(builtPrompt)
				}
				sh.resetRoundState()
				continue
			}
			break
		}
	}

	run()

	if keepAliveStop != nil {
		close(keepAliveStop)
	}

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

	// 2.5 同步 Warp 刷新令牌（如有变更）
	if currentAccount != nil && strings.EqualFold(currentAccount.AccountType, "warp") {
		if h.loadBalancer != nil && h.loadBalancer.Store != nil {
			if warpClient, ok := apiClient.(*warp.Client); ok {
				if warpClient.SyncAccountState() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := h.loadBalancer.Store.UpdateAccount(ctx, currentAccount); err != nil {
						slog.Warn("同步 Warp 账号令牌失败", "account", currentAccount.Name, "error", err)
					}
				}
			}
		}
	}

	// 3. Update account usage
	if currentAccount != nil && h.loadBalancer != nil {
		go func(accountID int64, inputTokens, outputTokens int) {
			usage := float64(inputTokens + outputTokens)
			if usage > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := h.loadBalancer.Store.IncrementUsage(ctx, accountID, usage); err != nil {
					slog.Error("Failed to update account usage", "account_id", accountID, "error", err)
				}
			}
		}(currentAccount.ID, sh.inputTokens, sh.outputTokens)
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


