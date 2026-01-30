package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/internal/tiktoken"
)

type Handler struct {
	config       *config.Config
	client       UpstreamClient
	loadBalancer *loadbalancer.LoadBalancer
	summaryCache prompt.SummaryCache
	summaryStats *summarycache.Stats
	summaryLog   bool
}

type UpstreamClient interface {
	SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(client.SSEMessage), logger *debug.Logger) error
}

type UpstreamPayloadClient interface {
	SendRequestWithPayload(ctx context.Context, req client.UpstreamRequest, onMessage func(client.SSEMessage), logger *debug.Logger) error
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

func New(cfg *config.Config) *Handler {
	return &Handler{
		config:     cfg,
		client:     client.New(cfg),
		summaryLog: cfg.SummaryCacheLog,
	}
}

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		config:       cfg,
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

func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ClaudeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
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

	// 选择账号
	var apiClient UpstreamClient
	var currentAccount *store.Account
	var failedAccountIDs []int64

	selectAccount := func() error {
		if h.loadBalancer != nil {
			targetChannel := h.loadBalancer.GetModelChannel(r.Context(), req.Model)
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
			apiClient = client.NewFromAccount(account, h.config)
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
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	if currentAccount != nil && h.loadBalancer != nil {
		h.loadBalancer.AcquireConnection(currentAccount.ID)
		defer h.loadBalancer.ReleaseConnection(currentAccount.ID)
	}

	conversationKey := conversationKeyForRequest(r, req)
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
	if gateNoTools {
		effectiveTools = nil
		slog.Info("tool_gate: disabled tools for short non-code request")
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

	// Apply Caching Strategy if enabled
	cacheStrategy := h.config.CacheStrategy
	if cacheStrategy != "" && cacheStrategy != "none" {
		// startCache := time.Now()
		applyCacheStrategy(&req, cacheStrategy)
		// log.Printf("Cache strategy applied in %v", time.Since(startCache))
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
	}
	builtPrompt := prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
		Model:    req.Model,
		Messages: req.Messages,
		System:   req.System,
		Tools:    effectiveTools,
		Stream:   req.Stream,
	}, opts)
	if h.config.DebugEnabled {
		log.Printf("[Performance] BuildPromptV2WithOptions took %v", time.Since(startBuild))
	}

	// ... rest of code

	if h.summaryStats != nil && h.summaryLog {
		hitsAfter, missesAfter := h.summaryStats.Snapshot()
		hitDelta := hitsAfter - hitsBefore
		missDelta := missesAfter - missesBefore
		if hitDelta > 0 || missDelta > 0 {
			slog.Info("summary_cache", "hit", hitDelta, "miss", missDelta)
		}
	}

	// 映射模型
	mappedModel := mapModel(req.Model)
	slog.Info("Model mapping", "original", req.Model, "mapped", mappedModel)

	useWS := strings.EqualFold(strings.TrimSpace(h.config.UpstreamMode), "ws")
	if toolCallMode == "internal" && req.Stream {
		req.Stream = false
	}

	isStream := req.Stream

	if isStream {
		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		if _, ok := w.(http.Flusher); !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
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
	if toolCallMode == "internal" && !useWS && allowBashName != "" && shouldPreflightTools(userText) {
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
	inputTokens := tiktoken.EstimateTextTokens(builtPrompt)

	sh := newStreamHandler(
		h.config, w, logger, toolCallMode, suppressThinking, allowedTools, allowedIndex, preflightResults, shouldLocalFallback, isStream,
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

	slog.Info("New request received")

	done := make(chan struct{})

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
				}
			}
		}()
	}

	go func() {
		defer close(done)
		for {
			sh.internalNeedsFollowup = false // Reset per retry/turn
			sh.internalToolResults = nil
			maxRetries := h.config.MaxRetries
			if maxRetries < 0 {
				maxRetries = 0
			}
			retryDelay := time.Duration(h.config.RetryDelay) * time.Millisecond
			retriesRemaining := maxRetries

			upstreamReq := client.UpstreamRequest{
				Prompt:      builtPrompt,
				ChatHistory: chatHistory,
				Model:       mappedModel,
				Messages:    upstreamMessages,
				System:      []prompt.SystemItem(req.System),
				Tools:       effectiveTools,
				NoTools:     gateNoTools,
				NoThinking:  suggestionMode,
			}
			for {
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
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
					slog.Warn("Account request failed, switching account", "account", currentAccount.Name, "failed_count", len(failedAccountIDs))
					if retryErr := selectAccount(); retryErr == nil {
						slog.Info("Switched to account", "account", currentAccount.Name)
					} else {
						slog.Error("No more accounts available", "error", retryErr)
						sh.finishResponse("end_turn")
						return
					}
				}
				if retryDelay > 0 {
					select {
					case <-time.After(retryDelay):
					case <-r.Context().Done():
						sh.finishResponse("end_turn")
						return
					}
				}
			}
			if (toolCallMode == "internal" || toolCallMode == "auto") && sh.internalNeedsFollowup {
				for _, result := range sh.internalToolResults {
					upstreamMessages = append(upstreamMessages,
						prompt.Message{
							Role: "assistant",
							Content: prompt.MessageContent{
								Blocks: []prompt.ContentBlock{
									{
										Type:  "tool_use",
										ID:    result.call.id,
										Name:  result.call.name,
										Input: result.input,
									},
								},
							},
						},
						prompt.Message{
							Role: "user",
							Content: prompt.MessageContent{
								Blocks: []prompt.ContentBlock{
									{
										Type:      "tool_result",
										ToolUseID: result.call.id,
										Content:   result.output,
										IsError:   result.isError,
									},
								},
							},
						},
					)
					chatHistory = append(chatHistory, map[string]interface{}{
						"Role": "assistant",
						"Content": []map[string]interface{}{
							{
								"Type":  "tool_use",
								"ID":    result.call.id,
								"Name":  result.call.name,
								"Input": result.input,
							},
						},
					})
					chatHistory = append(chatHistory, map[string]interface{}{
						"Role": "user",
						"Content": []map[string]interface{}{
							{
								"Type":      "tool_result",
								"ToolUseID": result.call.id,
								"Content":   result.output,
								"IsError":   result.isError,
							},
						},
					})
				}
				sh.resetRoundState()
				continue
			}
			break
		}
	}()

	<-done

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
			if blockType, ok := sh.contentBlocks[i]["type"].(string); ok && blockType == "text" {
				if builder, ok := sh.textBlockBuilders[i]; ok {
					sh.contentBlocks[i]["text"] = builder.String()
				} else if _, ok := sh.contentBlocks[i]["text"]; !ok {
					sh.contentBlocks[i]["text"] = ""
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
}
