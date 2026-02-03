package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

// resolveWorkdir determines the working directory from headers, system prompt, or session.
func (h *Handler) resolveWorkdir(r *http.Request, req ClaudeRequest) string {
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
		h.sessionWorkdirsMu.RLock()
		if val, ok := h.sessionWorkdirs[conversationKey]; ok {
			dynamicWorkdir = val
			slog.Info("Recovered workdir from session", "workdir", dynamicWorkdir, "session", conversationKey)
		}
		h.sessionWorkdirsMu.RUnlock()
	}

	// Persist for future turns in this session
	if dynamicWorkdir != "" {
		h.sessionWorkdirsMu.Lock()
		h.sessionWorkdirs[conversationKey] = dynamicWorkdir
		h.sessionWorkdirsMu.Unlock()
	}

	if dynamicWorkdir != "" {
		slog.Info("Using dynamic workdir", "workdir", dynamicWorkdir)
	}
	return dynamicWorkdir
}

// selectAccount logic extracted from HandleMessages
func (h *Handler) selectAccount(ctx context.Context, model, forcedChannel string, failedAccountIDs []int64) (UpstreamClient, *store.Account, error) {
	if h.loadBalancer != nil {
		targetChannel := forcedChannel
		if targetChannel == "" {
			targetChannel = h.loadBalancer.GetModelChannel(ctx, model)
		}
		if targetChannel != "" {
			slog.Info("Model recognition", "model", model, "channel", targetChannel)
		}
		account, err := h.loadBalancer.GetNextAccountExcludingByChannel(ctx, failedAccountIDs, targetChannel)
		if err != nil {
			if forcedChannel != "" {
				return nil, nil, err
			}
			if h.client != nil {
				slog.Info("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
				return h.client, nil, nil
			}
			return nil, nil, err
		}
		var client UpstreamClient
		if strings.EqualFold(account.AccountType, "warp") {
			client = warp.NewFromAccount(account, h.config)
		} else {
			orchidsClient := orchids.NewFromAccount(account, h.config)
			orchidsClient.SetFSExecutor(h.orchidsFSExecutor)
			client = orchidsClient
		}
		return client, account, nil
	} else if h.client != nil {
		return h.client, nil, nil
	}
	return nil, nil, errors.New("no client configured")
}

// executePreflightTools performs parallel preflight checks
func (h *Handler) executePreflightTools(toolCallMode, allowBashName, userText string) ([]safeToolResult, []interface{}) {
	if (toolCallMode == "internal" || toolCallMode == "auto") && allowBashName != "" && shouldPreflightTools(userText) {
		preflight := []string{
			"pwd",
			"find . -maxdepth 2 -not -path '*/.*'",
			"ls -la",
		}

		results := make([]safeToolResult, len(preflight))
		var wg sync.WaitGroup
		wg.Add(len(preflight))

		for i, cmd := range preflight {
			go func(i int, cmd string) {
				defer wg.Done()
				call := toolCall{
					id:    fmt.Sprintf("internal_tool_%d", i+1),
					name:  allowBashName,
					input: fmt.Sprintf(`{"command":%q,"description":"internal preflight"}`, cmd),
				}
				result := executeSafeTool(call)
				result.output = normalizeToolResultOutput(result.output)
				results[i] = result
			}(i, cmd)
		}
		wg.Wait()

		// Construct chat history (must be ordered to match execution order for consistency)
		// Since we filled 'results' by index, order is preserved.

		var chatHistory []interface{}
		// Pre-allocate assuming 2 entries per result
		chatHistory = make([]interface{}, 0, len(results)*2)

		for _, result := range results {
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
		return results, chatHistory
	}
	return nil, make([]interface{}, 0)
}

func (h *Handler) updateAccountStats(account *store.Account, inputTokens, outputTokens int) {
	if account == nil || h.loadBalancer == nil {
		return
	}
	go func(accountID int64, inputTokens, outputTokens int) {
		usage := float64(inputTokens + outputTokens)
		if usage > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Use the new batched method
			if err := h.loadBalancer.Store.IncrementAccountStats(ctx, accountID, usage, 1); err != nil {
				slog.Error("Failed to update account stats", "account_id", accountID, "error", err)
			}
		}
	}(account.ID, inputTokens, outputTokens)
}

func (h *Handler) syncWarpState(account *store.Account, client UpstreamClient) {
	if account != nil && strings.EqualFold(account.AccountType, "warp") {
		if h.loadBalancer != nil && h.loadBalancer.Store != nil {
			if warpClient, ok := client.(*warp.Client); ok {
				if warpClient.SyncAccountState() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := h.loadBalancer.Store.UpdateAccount(ctx, account); err != nil {
						slog.Warn("同步 Warp 账号令牌失败", "account", account.Name, "error", err)
					}
				}
			}
		}
	}
}
