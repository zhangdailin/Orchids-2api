package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

// resolveWorkdir determines the working directory from headers, system prompt, or session.
// 返回当前 workdir、上一轮 workdir、以及是否发生变更。
func (h *Handler) resolveWorkdir(r *http.Request, req ClaudeRequest, conversationKey string) (string, string, bool) {
	prevWorkdir := ""
	if conversationKey != "" {
		h.sessionWorkdirsMu.RLock()
		prevWorkdir = h.sessionWorkdirs[conversationKey]
		h.sessionWorkdirsMu.RUnlock()
	}

	// Prefer explicit workdir from request payload/header/system.
	dynamicWorkdir, source := extractWorkdirFromRequest(r, req)

	// Only recover from session when we have a stable explicit conversation key.
	hasExplicitSession := req.ConversationID != "" ||
		headerValue(r, "X-Conversation-Id", "X-Session-Id", "X-Thread-Id", "X-Chat-Id") != "" ||
		(req.Metadata != nil && metadataString(req.Metadata,
			"conversation_id", "conversationId",
			"session_id", "sessionId",
			"thread_id", "threadId",
			"chat_id", "chatId",
		) != "")

	if dynamicWorkdir == "" && hasExplicitSession && prevWorkdir != "" {
		dynamicWorkdir = prevWorkdir
		source = "session"
		slog.Info("Recovered workdir from session", "workdir", dynamicWorkdir, "session", conversationKey)
	}

	// Persist for future turns in this session
	if dynamicWorkdir != "" && conversationKey != "" {
		h.sessionWorkdirsMu.Lock()
		h.sessionWorkdirs[conversationKey] = dynamicWorkdir
		h.sessionLastAccess[conversationKey] = time.Now()
		h.cleanupSessionWorkdirsLocked()
		h.sessionWorkdirsMu.Unlock()
	}

	if dynamicWorkdir != "" {
		slog.Info("Using dynamic workdir", "workdir", dynamicWorkdir, "source", source)
	}
	rawPrev := strings.TrimSpace(prevWorkdir)
	rawNext := strings.TrimSpace(dynamicWorkdir)
	normalizedPrev := ""
	normalizedNext := ""
	if rawPrev != "" {
		normalizedPrev = filepath.Clean(rawPrev)
	}
	if rawNext != "" {
		normalizedNext = filepath.Clean(rawNext)
	}
	changed := normalizedPrev != "" && normalizedNext != "" && normalizedPrev != normalizedNext
	return dynamicWorkdir, prevWorkdir, changed
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
			client = orchidsClient
		}
		return client, account, nil
	} else if h.client != nil {
		return h.client, nil, nil
	}
	return nil, nil, errors.New("no client configured")
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

func (h *Handler) syncWarpState(account *store.Account, client UpstreamClient, snapshot *store.Account) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}

	var changed bool
	if strings.EqualFold(account.AccountType, "warp") {
		if warpClient, ok := client.(*warp.Client); ok {
			changed = warpClient.SyncAccountState()
		}
	} else if orchidsClient, ok := client.(*orchids.Client); ok {
		// Orchids 账号：通过快照比较检测 forceRefreshToken 是否更新了账号信息
		changed = orchidsClient.SyncAccountState(snapshot)
	}

	if changed {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.loadBalancer.Store.UpdateAccount(ctx, account); err != nil {
			slog.Warn("同步账号令牌失败", "account", account.Name, "type", account.AccountType, "error", err)
		}
	}
}

const (
	sessionMaxSize         = 1024
	sessionCleanupInterval = 5 * time.Minute
	sessionMaxAge          = 30 * time.Minute
)

// cleanupSessionWorkdirsLocked removes stale session entries.
// Must be called with sessionWorkdirsMu held for writing.
func (h *Handler) cleanupSessionWorkdirsLocked() {
	now := time.Now()
	if len(h.sessionWorkdirs) < sessionMaxSize && now.Sub(h.sessionCleanupRun) < sessionCleanupInterval {
		return
	}
	for key, lastAccess := range h.sessionLastAccess {
		if now.Sub(lastAccess) > sessionMaxAge {
			delete(h.sessionWorkdirs, key)
			delete(h.sessionConvIDs, key)
			delete(h.sessionLastAccess, key)
		}
	}
	h.sessionCleanupRun = now
}

type upstreamErrorClass struct {
	category      string
	retryable     bool
	switchAccount bool
}

func classifyUpstreamError(errStr string) upstreamErrorClass {
	lower := strings.ToLower(errStr)
	switch {
	case strings.Contains(lower, "context canceled") || strings.Contains(lower, "canceled"):
		return upstreamErrorClass{category: "canceled", retryable: false, switchAccount: false}
	case hasExplicitHTTPStatus(lower, "401") ||
		strings.Contains(lower, "signed out") || strings.Contains(lower, "signed_out"):
		return upstreamErrorClass{category: "auth", retryable: true, switchAccount: true}
	case hasExplicitHTTPStatus(lower, "403"):
		return upstreamErrorClass{category: "auth_blocked", retryable: true, switchAccount: true}
	case hasExplicitHTTPStatus(lower, "404"):
		return upstreamErrorClass{category: "auth_blocked", retryable: false, switchAccount: false}
	case strings.Contains(lower, "input is too long") || hasExplicitHTTPStatus(lower, "400"):
		return upstreamErrorClass{category: "client", retryable: false, switchAccount: false}
	case hasExplicitHTTPStatus(lower, "429") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "rate limit"):
		return upstreamErrorClass{category: "rate_limit", retryable: true, switchAccount: true}
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") || strings.Contains(lower, "context deadline"):
		return upstreamErrorClass{category: "timeout", retryable: true, switchAccount: true}
	case strings.Contains(lower, "connection reset") || strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "unexpected eof") || strings.Contains(lower, "use of closed") ||
		strings.Contains(lower, "broken pipe") || strings.HasSuffix(lower, ": eof") || lower == "eof":
		return upstreamErrorClass{category: "network", retryable: true, switchAccount: true}
	case hasExplicitHTTPStatus(lower, "500") || hasExplicitHTTPStatus(lower, "502") || hasExplicitHTTPStatus(lower, "503") || hasExplicitHTTPStatus(lower, "504"):
		return upstreamErrorClass{category: "server", retryable: true, switchAccount: true}
	default:
		return upstreamErrorClass{category: "unknown", retryable: true, switchAccount: true}
	}
}

func computeRetryDelay(base time.Duration, attempt int, category string) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	delay := base * time.Duration(1<<(attempt-1))
	if category == "rate_limit" && delay < 2*time.Second {
		delay = 2 * time.Second
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}
