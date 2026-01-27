package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/client"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/tiktoken"
)

type Handler struct {
	config       *config.Config
	client       UpstreamClient
	loadBalancer *loadbalancer.LoadBalancer
}

type UpstreamClient interface {
	SendRequest(ctx context.Context, prompt string, chatHistory []interface{}, model string, onMessage func(client.SSEMessage), logger *debug.Logger) error
}

type ClaudeRequest struct {
	Model    string              `json:"model"`
	Messages []prompt.Message    `json:"messages"`
	System   []prompt.SystemItem `json:"system"`
	Tools    []interface{}       `json:"tools"`
	Stream   bool                `json:"stream"`
}

func New(cfg *config.Config) *Handler {
	return &Handler{
		config: cfg,
		client: client.New(cfg),
	}
}

func NewWithLoadBalancer(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		config:       cfg,
		loadBalancer: lb,
	}
}

// mapModel 根据请求的 model 名称映射到实际使用的模型
func mapModel(requestModel string) string {
	lowerModel := strings.ToLower(requestModel)
	if strings.Contains(lowerModel, "opus") {
		return "claude-opus-4.5"
	}
	if strings.Contains(lowerModel, "haiku") {
		return "gemini-3-flash"
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
	logger := debug.New(h.config.DebugEnabled)
	defer logger.Close()

	// 1. 记录进入的 Claude 请求
	logger.LogIncomingRequest(req)

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
			apiClient = client.NewFromAccount(account)
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

	// 构建 prompt（V2 Markdown 格式）
	builtPrompt := prompt.BuildPromptV2(prompt.ClaudeAPIRequest{
		Model:    req.Model,
		Messages: req.Messages,
		System:   req.System,
		Tools:    req.Tools,
		Stream:   req.Stream,
	})

	// 2. 记录转换后的 prompt
	logger.LogConvertedPrompt(builtPrompt)

	// 映射模型
	mappedModel := mapModel(req.Model)
	log.Printf("模型映射: %s -> %s", req.Model, mappedModel)

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
	var finalStopReason string
	toolBlocks := make(map[string]int)
	var responseText strings.Builder
	var contentBlocks []map[string]interface{}
	var currentTextIndex = -1

	// Token 计数
	inputTokens := tiktoken.EstimateTextTokens(builtPrompt)
	var outputTokens int
	var outputMu sync.Mutex

	addOutputTokens := func(text string) {
		if text == "" {
			return
		}
		tokens := tiktoken.EstimateTextTokens(text)
		outputMu.Lock()
		outputTokens += tokens
		outputMu.Unlock()
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

	finishResponse := func(stopReason string) {
		mu.Lock()
		if hasReturn {
			mu.Unlock()
			return
		}
		hasReturn = true
		finalStopReason = stopReason
		mu.Unlock()

		if isStream {
			deltaData, _ := json.Marshal(map[string]interface{}{
				"type":  "message_delta",
				"delta": map[string]string{"stop_reason": stopReason},
				"usage": map[string]int{"output_tokens": outputTokens},
			})
			writeSSE("message_delta", string(deltaData))

			stopData, _ := json.Marshal(map[string]string{"type": "message_stop"})
			writeSSE("message_stop", string(stopData))
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
			err := apiClient.SendRequest(r.Context(), builtPrompt, []interface{}{}, mappedModel, func(msg client.SSEMessage) {
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
							"text": "",
						})
						currentTextIndex = len(contentBlocks) - 1
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
							if text, ok := contentBlocks[currentTextIndex]["text"].(string); ok {
								contentBlocks[currentTextIndex]["text"] = text + delta
							}
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
					mu.Lock()
					blockIndex++
					idx := blockIndex
					toolBlocks[toolID] = idx
					mu.Unlock()

				case "model.tool-input-delta":
					// 忽略，等待 tool-call

				case "model.tool-input-end":
					// 忽略，等待 tool-call

				case "model.tool-call":
					toolID, _ := msg.Event["toolCallId"].(string)
					toolName, _ := msg.Event["toolName"].(string)
					inputStr, _ := msg.Event["input"].(string)
					if toolID == "" {
						return
					}
					if !isStream {
						addOutputTokens(toolName)
						addOutputTokens(inputStr)
						fixedInput := fixToolInput(inputStr)
						var inputValue interface{}
						if err := json.Unmarshal([]byte(fixedInput), &inputValue); err != nil {
							inputValue = map[string]interface{}{}
						}
						contentBlocks = append(contentBlocks, map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  toolName,
							"input": inputValue,
						})
						return
					}

					mu.Lock()
					idx, exists := toolBlocks[toolID]
					mu.Unlock()
					if !exists {
						return
					}

					addOutputTokens(toolName)
					addOutputTokens(inputStr)
					fixedInput := fixToolInput(inputStr)

					// content_block_start
					startData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]interface{}{
							"type":  "tool_use",
							"id":    toolID,
							"name":  toolName,
							"input": map[string]interface{}{},
						},
					})
					writeSSE("content_block_start", string(startData))

					// content_block_delta
					deltaData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{
							"type":         "input_json_delta",
							"partial_json": fixedInput,
						},
					})
					writeSSE("content_block_delta", string(deltaData))

					// content_block_stop
					stopData, _ := json.Marshal(map[string]interface{}{
						"type":  "content_block_stop",
						"index": idx,
					})
					writeSSE("content_block_stop", string(stopData))

				case "model.finish":
					stopReason := "end_turn"
					if finishReason, ok := msg.Event["finishReason"].(string); ok {
						switch finishReason {
						case "tool-calls":
							stopReason = "tool_use"
						case "stop", "end_turn":
							stopReason = "end_turn"
						}
					}
					finishResponse(stopReason)
				}
			}, logger)

			if err != nil {
				log.Printf("Error: %v", err)
				if currentAccount != nil && h.loadBalancer != nil {
					failedAccountIDs = append(failedAccountIDs, currentAccount.ID)
					log.Printf("账号 %s 请求失败，尝试切换账号 (已排除 %d 个)", currentAccount.Name, len(failedAccountIDs))
					if retryErr := selectAccount(); retryErr == nil {
						log.Printf("切换到账号: %s，重新发送请求", currentAccount.Name)
						continue
					} else {
						log.Printf("无更多可用账号: %v", retryErr)
					}
				}
				finishResponse("end_turn")
			}
			break
		}
	}()

	<-done

	// 确保有最终响应
	if !hasReturn {
		finishResponse("end_turn")
	}

	if !isStream {
		stopReason := finalStopReason
		if stopReason == "" {
			stopReason = "end_turn"
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
