package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/summarycache"
	"orchids-api/internal/tiktoken"

	"github.com/kballard/go-shellquote"
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

// mapModel 根据请求的 model 名称映射到实际使用的模型
func mapModel(requestModel string) string {
	lowerModel := strings.ToLower(requestModel)
	if strings.Contains(lowerModel, "opus") {
		return "claude-opus-4.5"
	}
	if strings.Contains(lowerModel, "haiku") {
		return "claude-sonnet-4-5"
	}
	return "claude-sonnet-4-5"
}

// fixToolInput 修复工具输入中的类型问题
func fixToolInput(inputJSON string) string {
	if inputJSON == "" {
		return "{}"
	}

	var input map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return inputJSON
	}

	fixed := false
	for key, value := range input {
		if strVal, ok := value.(string); ok {
			strVal = strings.TrimSpace(strVal)

			if strVal == "true" {
				input[key] = true
				fixed = true
				continue
			} else if strVal == "false" {
				input[key] = false
				fixed = true
				continue
			}

			if num, err := strconv.ParseInt(strVal, 10, 64); err == nil {
				input[key] = num
				fixed = true
				continue
			}

			if fnum, err := strconv.ParseFloat(strVal, 64); err == nil {
				input[key] = fnum
				fixed = true
				continue
			}

			if (strings.HasPrefix(strVal, "[") && strings.HasSuffix(strVal, "]")) ||
				(strings.HasPrefix(strVal, "{") && strings.HasSuffix(strVal, "}")) {
				var parsed interface{}
				if err := json.Unmarshal([]byte(strVal), &parsed); err == nil {
					input[key] = parsed
					fixed = true
				}
			}
		}
	}

	if !fixed {
		return inputJSON
	}

	result, err := json.Marshal(input)
	if err != nil {
		return inputJSON
	}
	return string(result)
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
			account, err := h.loadBalancer.GetNextAccountExcluding(failedAccountIDs)
			if err != nil {
				if h.client != nil {
					apiClient = h.client
					currentAccount = nil
					log.Println("负载均衡无可用账号，使用默认配置")
					return nil
				}
				return err
			}
			log.Printf("使用账号: %s (%s)", account.Name, account.Email)
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
	gateNoTools := false
	effectiveTools := req.Tools
	if gateNoTools {
		effectiveTools = nil
		log.Printf("tool_gate: disabled tools for short non-code request")
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

	// 构建 prompt（V2 Markdown 格式）
	builtPrompt := prompt.BuildPromptV2WithOptions(prompt.ClaudeAPIRequest{
		Model:    req.Model,
		Messages: req.Messages,
		System:   req.System,
		Tools:    effectiveTools,
		Stream:   req.Stream,
	}, prompt.PromptOptions{
		MaxTokens:        h.config.ContextMaxTokens,
		SummaryMaxTokens: h.config.ContextSummaryMaxTokens,
		KeepTurns:        h.config.ContextKeepTurns,
		ConversationID:   conversationKey,
		SummaryCache:     h.summaryCache,
	})

	if h.summaryStats != nil && h.summaryLog {
		hitsAfter, missesAfter := h.summaryStats.Snapshot()
		hitDelta := hitsAfter - hitsBefore
		missDelta := missesAfter - missesBefore
		if hitDelta > 0 || missDelta > 0 {
			log.Printf("summary_cache: hit=%d miss=%d", hitDelta, missDelta)
		}
	}

	// 映射模型
	mappedModel := mapModel(req.Model)
	log.Printf("模型映射: %s -> %s", req.Model, mappedModel)

	useWS := strings.EqualFold(strings.TrimSpace(h.config.UpstreamMode), "ws")
	if toolCallMode == "internal" && req.Stream {
		req.Stream = false
	}

	isStream := req.Stream
	var flusher http.Flusher
	if isStream {
		// 设置 SSE 响应头
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		streamFlusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}
		flusher = streamFlusher
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	// 状态管理
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	blockIndex := -1
	var hasReturn bool
	var mu sync.Mutex
	var keepAliveStop chan struct{}
	writeKeepAlive := func() {
		if !isStream {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if hasReturn {
			return
		}
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		logger.LogOutputSSE("keepalive", "")
	}
	if isStream {
		keepAliveStop = make(chan struct{})
		ticker := time.NewTicker(keepAliveInterval)
		go func() {
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					writeKeepAlive()
				case <-keepAliveStop:
					return
				}
			}
		}()
	}
	var finalStopReason string
	toolBlocks := make(map[string]int)
	var responseText strings.Builder
	var contentBlocks []map[string]interface{}
	var currentTextIndex = -1
	textBlockBuilders := map[int]*strings.Builder{}
	var pendingToolCalls []toolCall
	toolInputNames := map[string]string{}
	toolInputBuffers := map[string]*strings.Builder{}
	toolInputHadDelta := map[string]bool{}
	toolCallHandled := map[string]bool{}
	currentToolInputID := ""
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
	var toolCallCount int
	var internalToolResults []safeToolResult
	var preflightResults []safeToolResult
	var internalNeedsFollowup bool
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
	var outputTokens int
	var outputMu sync.Mutex
	var outputBuilder strings.Builder
	var useUpstreamUsage bool
	outputTokenMode := strings.ToLower(strings.TrimSpace(h.config.OutputTokenMode))
	if outputTokenMode == "" {
		outputTokenMode = "final"
	}

	addOutputTokens := func(text string) {
		if text == "" {
			return
		}
		outputMu.Lock()
		if useUpstreamUsage {
			outputMu.Unlock()
			return
		}
		outputMu.Unlock()
		if outputTokenMode == "stream" {
			tokens := tiktoken.EstimateTextTokens(text)
			outputMu.Lock()
			outputTokens += tokens
			outputMu.Unlock()
			return
		}
		outputMu.Lock()
		outputBuilder.WriteString(text)
		outputMu.Unlock()
	}

	finalizeOutputTokens := func() {
		if outputTokenMode == "stream" {
			return
		}
		outputMu.Lock()
		if useUpstreamUsage {
			outputMu.Unlock()
			return
		}
		text := outputBuilder.String()
		outputTokens = tiktoken.EstimateTextTokens(text)
		outputMu.Unlock()
	}
	setUsageTokens := func(input, output int) {
		outputMu.Lock()
		if input >= 0 {
			inputTokens = input
		}
		if output >= 0 {
			outputTokens = output
		}
		useUpstreamUsage = true
		outputMu.Unlock()
	}

	resetRoundState := func() {
		blockIndex = -1
		toolBlocks = make(map[string]int)
		responseText = strings.Builder{}
		contentBlocks = nil
		currentTextIndex = -1
		textBlockBuilders = map[int]*strings.Builder{}
		pendingToolCalls = nil
		toolInputNames = map[string]string{}
		toolInputBuffers = map[string]*strings.Builder{}
		toolInputHadDelta = map[string]bool{}
		toolCallHandled = map[string]bool{}
		currentToolInputID = ""
		toolCallCount = 0
		outputTokens = 0
		outputBuilder = strings.Builder{}
		useUpstreamUsage = false
		finalStopReason = ""
	}

	// SSE 写入函数
	writeSSE := func(event, data string) {
		if !isStream {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if hasReturn {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()

		// 5. 记录输出给客户端的 SSE
		logger.LogOutputSSE(event, data)
	}

	writeFinalSSE := func(event, data string) {
		if !isStream {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()

		// 5. 记录输出给客户端的 SSE
		logger.LogOutputSSE(event, data)
	}

	shouldEmitToolCalls := func(stopReason string) bool {
		switch toolCallMode {
		case "proxy":
			return true
		case "auto":
			return stopReason == "tool_use"
		case "internal":
			return false
		default:
			return true
		}
	}

	emitToolCallNonStream := func(call toolCall) {
		addOutputTokens(call.name)
		addOutputTokens(call.input)
		fixedInput := fixToolInput(call.input)
		var inputValue interface{}
		if err := json.Unmarshal([]byte(fixedInput), &inputValue); err != nil {
			inputValue = map[string]interface{}{}
		}
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    call.id,
			"name":  call.name,
			"input": inputValue,
		})
	}

	emitToolCallStream := func(call toolCall, idx int, write func(event, data string)) {
		if call.id == "" {
			return
		}
		if idx < 0 {
			mu.Lock()
			blockIndex++
			idx = blockIndex
			mu.Unlock()
		}

		addOutputTokens(call.name)
		addOutputTokens(call.input)
		fixedInput := fixToolInput(call.input)

		startData, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    call.id,
				"name":  call.name,
				"input": map[string]interface{}{},
			},
		})
		write("content_block_start", string(startData))

		deltaData, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{
				"type":         "input_json_delta",
				"partial_json": fixedInput,
			},
		})
		write("content_block_delta", string(deltaData))

		stopData, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		})
		write("content_block_stop", string(stopData))
	}

	flushPendingToolCalls := func(stopReason string, write func(event, data string)) {
		if !shouldEmitToolCalls(stopReason) {
			return
		}

		mu.Lock()
		calls := make([]toolCall, len(pendingToolCalls))
		copy(calls, pendingToolCalls)
		pendingToolCalls = nil
		mu.Unlock()

		for _, call := range calls {
			if isStream {
				emitToolCallStream(call, -1, write)
			} else {
				emitToolCallNonStream(call)
			}
		}
	}

	overrideWithLocalContext := func() {
		if isStream || !shouldLocalFallback {
			return
		}
		pwd := extractPreflightPwd(preflightResults)
		currentText := strings.TrimSpace(responseText.String())
		if currentText != "" && pwd != "" && strings.Contains(currentText, pwd) {
			return
		}
		fallback := buildLocalFallbackResponse(preflightResults)
		if fallback == "" {
			return
		}
		responseText = strings.Builder{}
		responseText.WriteString(fallback)
		contentBlocks = []map[string]interface{}{
			{
				"type": "text",
				"text": fallback,
			},
		}
		textBlockBuilders = map[int]*strings.Builder{}
		currentTextIndex = -1
		outputBuilder = strings.Builder{}
		outputBuilder.WriteString(fallback)
		outputTokens = 0
	}

	finishResponse := func(stopReason string) {
		if stopReason == "tool_use" {
			mu.Lock()
			hasToolCalls := toolCallCount > 0 || len(pendingToolCalls) > 0
			mu.Unlock()
			if !hasToolCalls {
				stopReason = "end_turn"
			}
		}
		mu.Lock()
		if hasReturn {
			mu.Unlock()
			return
		}
		hasReturn = true
		finalStopReason = stopReason
		mu.Unlock()

		if isStream {
			flushPendingToolCalls(stopReason, writeFinalSSE)
			finalizeOutputTokens()
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]string{"stop_reason": stopReason},
				"usage": map[string]int{"output_tokens": outputTokens},
			})
			writeFinalSSE("message_delta", string(deltaData))

			stopData, _ := json.Marshal(map[string]string{"type": "message_stop"})
			writeFinalSSE("message_stop", string(stopData))
		} else {
			flushPendingToolCalls(stopReason, writeFinalSSE)
			overrideWithLocalContext()
			finalizeOutputTokens()
		}

		// 6. 记录摘要
		logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), stopReason)
		log.Printf("请求完成: 输入=%d tokens, 输出=%d tokens, 耗时=%v", inputTokens, outputTokens, time.Since(startTime))
	}

	// 发送 message_start
	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   req.Model,
			"usage":   map[string]int{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
	writeSSE("message_start", string(startData))

	log.Println("新请求进入")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			internalNeedsFollowup = false
			internalToolResults = nil
			maxRetries := h.config.MaxRetries
			if maxRetries < 0 {
				maxRetries = 0
			}
			retryDelay := time.Duration(h.config.RetryDelay) * time.Millisecond
			retriesRemaining := maxRetries
			handleToolCall := func(call toolCall) {
				if call.id == "" {
					return
				}
				if toolCallMode == "internal" {
					result := executeSafeTool(call)
					internalToolResults = append(internalToolResults, result)
					return
				}
				if toolCallMode == "auto" {
					if !isStream {
						emitToolCallNonStream(call)
					}
					result := executeToolCall(call, h.config)
					internalToolResults = append(internalToolResults, result)
					return
				}
				mu.Lock()
				toolCallCount++
				mu.Unlock()
				if toolCallMode != "proxy" {
					mu.Lock()
					pendingToolCalls = append(pendingToolCalls, call)
					mu.Unlock()
					return
				}

				if !isStream {
					emitToolCallNonStream(call)
					return
				}

				idx := -1
				mu.Lock()
				if value, exists := toolBlocks[call.id]; exists {
					idx = value
					delete(toolBlocks, call.id)
				}
				mu.Unlock()
				emitToolCallStream(call, idx, writeSSE)
			}

			handleMessage := func(msg client.SSEMessage) {
				mu.Lock()
				if hasReturn {
					mu.Unlock()
					return
				}
				mu.Unlock()

				eventKey := msg.Type
				if msg.Type == "model" && msg.Event != nil {
					if evtType, ok := msg.Event["type"].(string); ok {
						eventKey = "model." + evtType
					}
				}
				getUsageInt := func(usage map[string]interface{}, key string) (int, bool) {
					if usage == nil {
						return 0, false
					}
					if raw, ok := usage[key]; ok {
						switch v := raw.(type) {
						case float64:
							return int(v), true
						case int:
							return v, true
						case json.Number:
							if n, err := v.Int64(); err == nil {
								return int(n), true
							}
						}
					}
					return 0, false
				}

				switch eventKey {
				case "model.reasoning-start":
					mu.Lock()
					blockIndex++
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":          "content_block_start",
						"index":         idx,
						"content_block": map[string]string{"type": "thinking", "thinking": ""},
					})
					writeSSE("content_block_start", string(data))

				case "model.reasoning-delta":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					delta, _ := msg.Event["delta"].(string)
					if isStream {
						addOutputTokens(delta)
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]string{"type": "thinking_delta", "thinking": delta},
					})
					writeSSE("content_block_delta", string(data))

				case "model.reasoning-end":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(data))

				case "model.text-start":
					mu.Lock()
					blockIndex++
					idx := blockIndex
					mu.Unlock()
					if !isStream {
						contentBlocks = append(contentBlocks, map[string]interface{}{
							"type": "text",
						})
						currentTextIndex = len(contentBlocks) - 1
						textBlockBuilders[currentTextIndex] = &strings.Builder{}
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":          "content_block_start",
						"index":         idx,
						"content_block": map[string]string{"type": "text", "text": ""},
					})
					writeSSE("content_block_start", string(data))

				case "model.text-delta":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					delta, _ := msg.Event["delta"].(string)
					addOutputTokens(delta)
					if !isStream {
						responseText.WriteString(delta)
						if currentTextIndex >= 0 && currentTextIndex < len(contentBlocks) {
							builder, ok := textBlockBuilders[currentTextIndex]
							if !ok {
								builder = &strings.Builder{}
								textBlockBuilders[currentTextIndex] = builder
							}
							builder.WriteString(delta)
						}
					}
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]string{"type": "text_delta", "text": delta},
					})
					writeSSE("content_block_delta", string(data))

				case "model.text-end":
					mu.Lock()
					idx := blockIndex
					mu.Unlock()
					data, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(data))

				case "model.tool-input-start":
					toolID, _ := msg.Event["id"].(string)
					toolName, _ := msg.Event["toolName"].(string)
					if toolID == "" || toolName == "" {
						return
					}
					currentToolInputID = toolID
					toolInputNames[toolID] = toolName
					toolInputBuffers[toolID] = &strings.Builder{}
					toolInputHadDelta[toolID] = false
					if !isStream || (toolCallMode != "proxy" && toolCallMode != "auto") {
						return
					}
					mappedName := mapOrchidsToolName(toolName, "", allowedIndex, allowedTools)
					finalName, ok := resolveToolName(mappedName)
					if !ok {
						return
					}
					addOutputTokens(finalName)
					mu.Lock()
					blockIndex++
					idx := blockIndex
					toolBlocks[toolID] = idx
					toolCallCount++
					mu.Unlock()
					startData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  finalName,
							"input": map[string]interface{}{},
						},
					})
					writeSSE("content_block_start", string(startData))

				case "model.tool-input-delta":
					toolID, _ := msg.Event["id"].(string)
					delta, _ := msg.Event["delta"].(string)
					if toolID == "" {
						return
					}
					if buf, ok := toolInputBuffers[toolID]; ok {
						buf.WriteString(delta)
					}
					if delta != "" {
						toolInputHadDelta[toolID] = true
					}
					if !isStream || (toolCallMode != "proxy" && toolCallMode != "auto") || delta == "" {
						return
					}
					idx, ok := toolBlocks[toolID]
					if !ok {
						return
					}
					deltaData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": delta,
						},
					})
					writeSSE("content_block_delta", string(deltaData))

				case "model.tool-input-end":
					toolID, _ := msg.Event["id"].(string)
					if toolID == "" {
						return
					}
					if currentToolInputID == toolID {
						currentToolInputID = ""
					}
					name, ok := toolInputNames[toolID]
					if !ok || name == "" {
						delete(toolInputBuffers, toolID)
						delete(toolInputHadDelta, toolID)
						delete(toolInputNames, toolID)
						return
					}
					inputStr := ""
					if buf, ok := toolInputBuffers[toolID]; ok {
						inputStr = strings.TrimSpace(buf.String())
					}
					if isStream && (toolCallMode == "proxy" || toolCallMode == "auto") {
						if inputStr != "" {
							addOutputTokens(inputStr)
						}
						if idx, ok := toolBlocks[toolID]; ok {
							if !toolInputHadDelta[toolID] && inputStr != "" {
								deltaData, _ := json.Marshal(map[string]interface{}{
									"type":  "content_block_delta",
									"index": idx,
									"delta": map[string]interface{}{
										"type":         "input_json_delta",
										"partial_json": inputStr,
									},
								})
								writeSSE("content_block_delta", string(deltaData))
							}
							stopData, _ := json.Marshal(map[string]interface{}{
								"type":  "content_block_stop",
								"index": idx,
							})
							writeSSE("content_block_stop", string(stopData))
							delete(toolBlocks, toolID)
						}
					}
					delete(toolInputBuffers, toolID)
					delete(toolInputHadDelta, toolID)
					delete(toolInputNames, toolID)
					if toolCallHandled[toolID] {
						return
					}
					resolvedName := mapOrchidsToolName(name, inputStr, allowedIndex, allowedTools)
					finalName, ok := resolveToolName(resolvedName)
					if !ok {
						return
					}
					call := toolCall{id: toolID, name: finalName, input: inputStr}
					toolCallHandled[toolID] = true
					if toolCallMode == "proxy" && isStream {
						return
					}
					handleToolCall(call)

				case "model.tool-call":
					toolID, _ := msg.Event["toolCallId"].(string)
					toolName, _ := msg.Event["toolName"].(string)
					inputStr, _ := msg.Event["input"].(string)
					if toolID == "" {
						return
					}
					if currentToolInputID != "" && toolID != currentToolInputID {
						return
					}
					if toolCallHandled[toolID] {
						return
					}
					if _, ok := toolInputBuffers[toolID]; ok {
						return
					}
					mappedName := mapOrchidsToolName(toolName, inputStr, allowedIndex, allowedTools)
					resolvedName, ok := resolveToolName(mappedName)
					if !ok {
						return
					}
					call := toolCall{id: toolID, name: resolvedName, input: inputStr}
					toolCallHandled[toolID] = true
					handleToolCall(call)

				case "model.tokens-used":
					usage := msg.Event
					inputTokens, hasIn := getUsageInt(usage, "inputTokens")
					outputTokens, hasOut := getUsageInt(usage, "outputTokens")
					if !hasIn {
						inputTokens, hasIn = getUsageInt(usage, "input_tokens")
					}
					if !hasOut {
						outputTokens, hasOut = getUsageInt(usage, "output_tokens")
					}
					if hasIn || hasOut {
						in := -1
						out := -1
						if hasIn {
							in = inputTokens
						}
						if hasOut {
							out = outputTokens
						}
						setUsageTokens(in, out)
					}
					return

				case "model.finish":
					stopReason := "end_turn"
					if usage, ok := msg.Event["usage"].(map[string]interface{}); ok {
						inputTokens, hasIn := getUsageInt(usage, "inputTokens")
						outputTokens, hasOut := getUsageInt(usage, "outputTokens")
						if !hasIn {
							inputTokens, hasIn = getUsageInt(usage, "input_tokens")
						}
						if !hasOut {
							outputTokens, hasOut = getUsageInt(usage, "output_tokens")
						}
						if hasIn || hasOut {
							in := -1
							out := -1
							if hasIn {
								in = inputTokens
							}
							if hasOut {
								out = outputTokens
							}
							setUsageTokens(in, out)
						}
					}
					if finishReason, ok := msg.Event["finishReason"].(string); ok {
						switch finishReason {
						case "tool-calls":
							stopReason = "tool_use"
						case "stop", "end_turn":
							stopReason = "end_turn"
						}
					}
					if stopReason == "tool_use" && (toolCallMode == "internal" || toolCallMode == "auto") {
						if len(internalToolResults) > 0 {
							internalNeedsFollowup = true
						}
						return
					}
					finishResponse(stopReason)
				}
			}

			upstreamReq := client.UpstreamRequest{
				Prompt:      builtPrompt,
				ChatHistory: chatHistory,
				Model:       mappedModel,
				Messages:    upstreamMessages,
				System:      []prompt.SystemItem(req.System),
				Tools:       effectiveTools,
				NoTools:     gateNoTools,
			}
			for {
				var err error
				if sender, ok := apiClient.(UpstreamPayloadClient); ok {
					err = sender.SendRequestWithPayload(r.Context(), upstreamReq, handleMessage, logger)
				} else {
					err = apiClient.SendRequest(r.Context(), builtPrompt, chatHistory, mappedModel, handleMessage, logger)
				}

				if err == nil {
					break
				}

				log.Printf("Error: %v", err)
				if r.Context().Err() != nil {
					finishResponse("end_turn")
					return
				}
				if retriesRemaining <= 0 {
					if currentAccount != nil && h.loadBalancer != nil {
						log.Printf("账号 %s 请求失败，已达最大重试次数", currentAccount.Name)
					}
					finishResponse("end_turn")
					return
				}
				retriesRemaining--
				if currentAccount != nil && h.loadBalancer != nil {
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
					log.Printf("账号 %s 请求失败，尝试切换账号 (已排除 %d 个)", currentAccount.Name, len(failedAccountIDs))
					if retryErr := selectAccount(); retryErr == nil {
						log.Printf("切换到账号: %s，重新发送请求", currentAccount.Name)
					} else {
						log.Printf("无更多可用账号: %v", retryErr)
						finishResponse("end_turn")
						return
					}
				}
				if retryDelay > 0 {
					select {
					case <-time.After(retryDelay):
					case <-r.Context().Done():
						finishResponse("end_turn")
						return
					}
				}
			}
			if (toolCallMode == "internal" || toolCallMode == "auto") && internalNeedsFollowup {
				for _, result := range internalToolResults {
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
				resetRoundState()
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
	if !hasReturn {
		finishResponse("end_turn")
	}

	if !isStream {
		stopReason := finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}

		for i := range contentBlocks {
			if blockType, ok := contentBlocks[i]["type"].(string); ok && blockType == "text" {
				if builder, ok := textBlockBuilders[i]; ok {
					contentBlocks[i]["text"] = builder.String()
				} else if _, ok := contentBlocks[i]["text"]; !ok {
					contentBlocks[i]["text"] = ""
				}
			}
		}

		if len(contentBlocks) == 0 && responseText.Len() > 0 {
			contentBlocks = append(contentBlocks, map[string]interface{}{
				"type": "text",
				"text": responseText.String(),
			})
		}

		response := map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       contentBlocks,
			"model":         req.Model,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			},
		}
		_ = json.NewEncoder(w).Encode(response)
	}
	_ = finalStopReason
}

func conversationKeyForRequest(r *http.Request, req ClaudeRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "conversation_id", "conversationId", "session_id", "sessionId", "thread_id", "threadId", "chat_id", "chatId"); key != "" {
			return key
		}
	}
	if key := headerValue(r, "X-Conversation-Id", "X-Session-Id", "X-Thread-Id", "X-Chat-Id"); key != "" {
		return key
	}

	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if host == "" {
		return ""
	}
	ua := strings.TrimSpace(r.UserAgent())
	if ua == "" {
		return host
	}
	return host + "|" + ua
}

func metadataString(metadata map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str, ok := value.(string); ok {
				str = strings.TrimSpace(str)
				if str != "" {
					return str
				}
			}
		}
	}
	return ""
}

func headerValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func extractUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			return strings.TrimSpace(msg.Content.GetText())
		}
		var parts []string
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" {
				text := strings.TrimSpace(block.Text)
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func isPlanMode(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			if containsPlanReminder(msg.Content.GetText()) {
				return true
			}
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "text" && containsPlanReminder(block.Text) {
				return true
			}
		}
	}
	return false
}

func containsPlanReminder(text string) bool {
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "<system-reminder>") {
		return false
	}
	if strings.Contains(lower, "plan mode") || strings.Contains(lower, "planning") || strings.Contains(lower, "plan") {
		return true
	}
	if strings.Contains(text, "计划模式") || strings.Contains(text, "计划") || strings.Contains(text, "规划") {
		return true
	}
	return false
}

func isCommandPrefixRequest(req ClaudeRequest) (bool, string) {
	userText := extractUserText(req.Messages)
	if userText == "" {
		return false, ""
	}
	lower := strings.ToLower(userText)
	if !strings.Contains(lower, "<policy_spec>") && !strings.Contains(lower, "command prefix") {
		return false, ""
	}
	command := extractCommandFromPolicy(userText)
	if command == "" {
		return false, ""
	}
	return true, command
}

func extractCommandFromPolicy(text string) string {
	re := regexp.MustCompile(`(?m)^Command:\s*(.+)$`)
	if match := re.FindStringSubmatch(text); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	if idx := strings.Index(text, "Command:"); idx >= 0 {
		return strings.TrimSpace(text[idx+len("Command:"):])
	}
	return ""
}

func writeCommandPrefixResponse(w http.ResponseWriter, req ClaudeRequest, prefix string, startTime time.Time, logger *debug.Logger) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "none"
	}
	inputTokens := tiktoken.EstimateTextTokens(extractUserText(req.Messages))
	outputTokens := tiktoken.EstimateTextTokens(prefix)
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		write := func(event string, data string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			flusher.Flush()
			if logger != nil {
				logger.LogOutputSSE(event, data)
			}
		}

		startData, _ := json.Marshal(map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []interface{}{},
				"model":   req.Model,
				"usage":   map[string]int{"input_tokens": inputTokens, "output_tokens": 0},
			},
		})
		write("message_start", string(startData))

		blockStart, _ := json.Marshal(map[string]interface{}{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		write("content_block_start", string(blockStart))

		blockDelta, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": prefix},
		})
		write("content_block_delta", string(blockDelta))

		blockStop, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
		write("content_block_stop", string(blockStop))

		msgDelta, _ := json.Marshal(map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]string{"stop_reason": "end_turn"},
			"usage": map[string]int{"output_tokens": outputTokens},
		})
		write("message_delta", string(msgDelta))

		msgStop, _ := json.Marshal(map[string]string{"type": "message_stop"})
		write("message_stop", string(msgStop))
		if logger != nil {
			logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       []map[string]string{{"type": "text", "text": prefix}},
		"model":         req.Model,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
	_ = json.NewEncoder(w).Encode(response)
	if logger != nil {
		logger.LogSummary(inputTokens, outputTokens, time.Since(startTime), "end_turn")
	}
}

func detectCommandPrefix(command string) string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "none"
	}
	if looksLikeCommandInjection(trimmed) {
		return "command_injection_detected"
	}
	tokens, err := shellquote.Split(trimmed)
	if err != nil || len(tokens) == 0 {
		return "command_injection_detected"
	}

	var prefix []string
	i := 0
	for i < len(tokens) && isEnvAssignment(tokens[i]) {
		prefix = append(prefix, tokens[i])
		i++
	}
	if i >= len(tokens) {
		return "none"
	}

	cmdIndex := i
	cmd := tokens[i]
	i++
	lowerCmd := strings.ToLower(cmd)

	switch lowerCmd {
	case "git":
		subIdx := findGitSubcommandIndex(tokens, i)
		if subIdx == -1 {
			return "none"
		}
		sub := strings.ToLower(tokens[subIdx])
		if sub == "push" && subIdx == len(tokens)-1 {
			return "none"
		}
		prefix = append(prefix, tokens[cmdIndex:subIdx+1]...)
		return strings.Join(prefix, " ")
	case "go", "gg", "potion", "pig":
		if i >= len(tokens) {
			return "none"
		}
		prefix = append(prefix, tokens[cmdIndex:i+1]...)
		return strings.Join(prefix, " ")
	case "npm":
		if i >= len(tokens) {
			return "none"
		}
		sub := strings.ToLower(tokens[i])
		switch sub {
		case "run":
			if i+1 >= len(tokens) {
				return "none"
			}
			if i+2 >= len(tokens) && len(prefix) == 0 {
				return "none"
			}
			prefix = append(prefix, tokens[cmdIndex:i+2]...)
			return strings.Join(prefix, " ")
		case "test":
			if i+1 >= len(tokens) {
				return "none"
			}
			prefix = append(prefix, tokens[cmdIndex:i+1]...)
			return strings.Join(prefix, " ")
		default:
			prefix = append(prefix, tokens[cmdIndex])
			return strings.Join(prefix, " ")
		}
	default:
		prefix = append(prefix, tokens[cmdIndex])
		return strings.Join(prefix, " ")
	}
}

func isEnvAssignment(token string) bool {
	if token == "" {
		return false
	}
	if !envAssignPattern.MatchString(token) {
		return false
	}
	return true
}

var envAssignPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func looksLikeCommandInjection(command string) bool {
	if strings.Contains(command, "\n") || strings.Contains(command, "\r") {
		return true
	}
	if strings.Contains(command, "`") || strings.Contains(command, "$(") {
		return true
	}
	if strings.Contains(command, ";") || strings.Contains(command, "||") || strings.Contains(command, "&&") {
		return true
	}
	if strings.Contains(command, "|") {
		return true
	}
	return false
}

func findGitSubcommandIndex(tokens []string, start int) int {
	for i := start; i < len(tokens); i++ {
		if gitSubcommands[strings.ToLower(tokens[i])] {
			return i
		}
	}
	return -1
}

var gitSubcommands = map[string]bool{
	"add":      true,
	"branch":   true,
	"checkout": true,
	"clone":    true,
	"commit":   true,
	"diff":     true,
	"fetch":    true,
	"log":      true,
	"merge":    true,
	"pull":     true,
	"push":     true,
	"rebase":   true,
	"reset":    true,
	"show":     true,
	"stash":    true,
	"status":   true,
	"tag":      true,
	"remote":   true,
}

type toolNameInfo struct {
	name    string
	lowered string
	short   string
	props   map[string]struct{}
}

func normalizeToolName(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	lowered := strings.ToLower(raw)
	short := lowered
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '.' || r == '/' || r == ':'
	})
	if len(parts) > 0 {
		short = strings.ToLower(parts[len(parts)-1])
	}
	return lowered, short
}

func buildToolNameIndex(tools []interface{}, allowed map[string]string) []toolNameInfo {
	if len(tools) == 0 {
		return nil
	}
	index := make([]toolNameInfo, 0, len(tools))
	for _, tool := range tools {
		tm, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[strings.ToLower(name)]; !ok {
				continue
			}
		}
		lowered, short := normalizeToolName(name)
		if lowered == "" {
			continue
		}
		props := map[string]struct{}{}
		if schema, ok := tm["input_schema"].(map[string]interface{}); ok {
			if properties, ok := schema["properties"].(map[string]interface{}); ok {
				for key := range properties {
					props[key] = struct{}{}
				}
			}
		}
		index = append(index, toolNameInfo{
			name:    name,
			lowered: lowered,
			short:   short,
			props:   props,
		})
	}
	return index
}

func mapOrchidsToolName(raw string, inputStr string, index []toolNameInfo, allowed map[string]string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(allowed) == 0 {
		return raw
	}
	lowered, short := normalizeToolName(raw)
	if lowered == "" {
		return raw
	}
	if resolved, ok := allowed[lowered]; ok {
		return resolved
	}
	for _, info := range index {
		if info.short == short {
			return info.name
		}
	}

	aliasCandidates := map[string][]string{
		"ripgrep":                           {"grep", "ripgrep", "rg", "search_files"},
		"grep":                              {"grep", "ripgrep", "rg", "search_files"},
		"glob":                              {"glob", "list_files"},
		"ls":                                {"ls", "list", "list_files"},
		"read":                              {"read", "readfile", "read_file", "view"},
		"write":                             {"write", "writefile", "write_file", "create_file", "createfile", "save-file"},
		"edit":                              {"edit", "editfile", "edit_file", "str-replace-editor", "apply_diff"},
		"bash":                              {"runcommand", "run_command", "bash", "execute_command", "launch-process"},
		"todowrite":                         {"update_todo_list", "todo", "todo_write", "todowrite"},
		"askuserquestion":                   {"ask_followup_question", "ask"},
		"enterplanmode":                     {"enter_plan_mode", "enterplanmode"},
		"exitplanmode":                      {"exit_plan_mode", "exitplanmode"},
		"task":                              {"task", "new_task"},
		"taskoutput":                        {"task_output", "taskoutput"},
		"taskstop":                          {"task_stop", "taskstop"},
		"skill":                             {"skill", "use_skill"},
		"webfetch":                          {"web_fetch", "webfetch", "fetch"},
		"websearch":                         {"web_search", "websearch", "search"},
		"mcp__context7__query-docs":         {"query-docs"},
		"mcp__context7__resolve-library-id": {"resolve-library-id"},
	}

	if aliases, ok := aliasCandidates[short]; ok {
		for _, alias := range aliases {
			for _, info := range index {
				if info.short == alias {
					return info.name
				}
			}
		}
	}
	for _, info := range index {
		if aliases, ok := aliasCandidates[info.short]; ok {
			for _, alias := range aliases {
				if alias == short || alias == lowered {
					return info.name
				}
			}
		}
	}

	inputKeys := parseToolInputKeys(inputStr)
	if len(inputKeys) > 0 {
		var bestName string
		bestScore := -1
		bestExtra := 0
		for _, info := range index {
			if len(info.props) == 0 {
				continue
			}
			score := 0
			for _, key := range inputKeys {
				if _, ok := info.props[key]; ok {
					score++
				} else {
					score = -1
					break
				}
			}
			if score < 0 {
				continue
			}
			extra := len(info.props) - score
			if score > bestScore || (score == bestScore && extra < bestExtra) {
				bestScore = score
				bestExtra = extra
				bestName = info.name
			}
		}
		if bestName != "" {
			return bestName
		}
	}
	return raw
}

func parseToolInputKeys(inputStr string) []string {
	inputStr = strings.TrimSpace(inputStr)
	if inputStr == "" {
		return nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(inputStr), &payload); err != nil {
		return nil
	}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	return keys
}

func shouldPreflightTools(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "project") || strings.Contains(lower, "repo") || strings.Contains(lower, "directory") || strings.Contains(lower, "structure") {
		return true
	}
	if strings.Contains(text, "项目") || strings.Contains(text, "目录") || strings.Contains(text, "路径") || strings.Contains(text, "结构") {
		return true
	}
	return false
}

var fileHintRe = regexp.MustCompile(`(?i)\b[\w\-.]+\.(go|js|ts|tsx|jsx|py|java|c|cc|cpp|h|hpp|rs|rb|php|cs|html|css|json|yaml|yml|md|txt|log|csv|toml|ini)\b`)

func shouldGateTools(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if utf8.RuneCountInString(text) > 20 {
		return false
	}
	if containsFileHint(text) {
		return false
	}
	if containsCodeTaskHint(text) {
		return false
	}
	return true
}

func containsFileHint(text string) bool {
	if strings.Contains(text, "./") || strings.Contains(text, "../") {
		return true
	}
	if strings.ContainsAny(text, `/\`) {
		return true
	}
	return fileHintRe.MatchString(text)
}

func containsCodeTaskHint(text string) bool {
	lower := strings.ToLower(text)
	enHints := []string{
		"bug", "error", "issue", "fix", "broken", "panic", "exception", "stack", "trace",
		"compile", "build", "test", "lint", "format", "refactor",
		"function", "class", "method", "api", "endpoint", "request", "response",
		"performance", "optimize", "latency",
	}
	for _, hint := range enHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	cnHints := []string{"报错", "错误", "修复", "异常", "崩溃", "编译", "构建", "测试", "性能", "优化", "代码", "接口", "请求", "响应", "函数", "类", "方法", "项目", "分析", "架构", "设计", "文档"}
	for _, hint := range cnHints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

func filterSupportedTools(tools []interface{}) []interface{} {
	if len(tools) == 0 {
		return tools
	}
	supported := map[string]bool{
		"read":      true,
		"write":     true,
		"edit":      true,
		"bash":      true,
		"glob":      true,
		"grep":      true,
		"ls":        true,
		"list":      true,
		"todowrite": true,
	}
	var filtered []interface{}
	for _, tool := range tools {
		tm, ok := tool.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		if supported[strings.ToLower(strings.TrimSpace(name))] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func formatLocalToolResults(results []safeToolResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("以下 tool_result 来自用户本地环境，具有权威性；回答必须以此为准，不要使用你自己的环境推断。")
	for _, result := range results {
		inputJSON, _ := json.Marshal(result.input)
		errorAttr := ""
		if result.isError {
			errorAttr = ` is_error="true"`
		}
		b.WriteString("\n<tool_use id=\"")
		b.WriteString(result.call.id)
		b.WriteString("\" name=\"")
		b.WriteString(result.call.name)
		b.WriteString("\">\n")
		b.WriteString(string(inputJSON))
		b.WriteString("\n</tool_use>\n<tool_result tool_use_id=\"")
		b.WriteString(result.call.id)
		b.WriteString("\"")
		b.WriteString(errorAttr)
		b.WriteString(">\n")
		b.WriteString(result.output)
		b.WriteString("\n</tool_result>")
	}
	return b.String()
}

func injectLocalContext(promptText string, context string) string {
	context = strings.TrimSpace(context)
	if context == "" {
		return promptText
	}
	section := "<local_context>\n" + context + "\n</local_context>\n\n"
	marker := "<user_request>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return promptText[:idx] + section + promptText[idx:]
	}
	if strings.TrimSpace(promptText) == "" {
		return section
	}
	return promptText + "\n\n" + strings.TrimRight(section, "\n")
}

func injectToolGate(promptText string, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return promptText
	}
	section := "<tool_gate>\n" + message + "\n</tool_gate>\n\n"
	marker := "<user_request>"
	if idx := strings.Index(promptText, marker); idx != -1 {
		return promptText[:idx] + section + promptText[idx:]
	}
	if strings.TrimSpace(promptText) == "" {
		return section
	}
	return promptText + "\n\n" + strings.TrimRight(section, "\n")
}

func extractPreflightPwd(results []safeToolResult) string {
	if len(results) == 0 {
		return ""
	}
	if results[0].isError {
		return ""
	}
	return strings.TrimSpace(results[0].output)
}

func buildLocalFallbackResponse(results []safeToolResult) string {
	pwd := extractPreflightPwd(results)
	findOutput := ""
	if len(results) > 1 && !results[1].isError {
		findOutput = results[1].output
	}
	topEntries := extractTopLevelEntries(findOutput, 12)
	projectName := ""
	if pwd != "" {
		projectName = filepath.Base(pwd)
	}

	var b strings.Builder
	if projectName != "" {
		b.WriteString("这是 `")
		b.WriteString(projectName)
		b.WriteString("` 项目。")
	} else {
		b.WriteString("这是当前目录下的项目。")
	}
	if pwd != "" {
		b.WriteString("\n当前项目目录: ")
		b.WriteString(pwd)
	}
	if len(topEntries) > 0 {
		b.WriteString("\n顶层目录/文件: ")
		b.WriteString(strings.Join(topEntries, ", "))
	}
	if containsEntry(topEntries, "go.mod") {
		b.WriteString("\n从目录结构看，这是一个 Go 项目（包含 go.mod/go.sum、cmd、internal、web 等）。")
	} else if len(topEntries) > 0 {
		b.WriteString("\n从目录结构看，这是一个后端服务项目，建议查看 README.md 获取完整说明。")
	}
	b.WriteString("\n如需更准确的项目介绍，请告知你想关注的模块或具体文件。")
	return b.String()
}

func extractTopLevelEntries(findOutput string, limit int) []string {
	if findOutput == "" {
		return nil
	}
	seen := map[string]bool{}
	var entries []string
	for _, line := range strings.Split(findOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "." {
			continue
		}
		if strings.HasPrefix(line, "./") {
			line = strings.TrimPrefix(line, "./")
		}
		if line == "" || strings.Contains(line, "/") {
			continue
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		entries = append(entries, line)
		if limit > 0 && len(entries) >= limit {
			break
		}
	}
	return entries
}

func containsEntry(entries []string, target string) bool {
	for _, entry := range entries {
		if entry == target {
			return true
		}
	}
	return false
}
