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

	// Check for dynamic workdir header EARLY
	dynamicWorkdir := r.Header.Get("X-Orchids-Workdir")
	source := ""
	if dynamicWorkdir != "" {
		source = "header"
	}
	if dynamicWorkdir == "" {
		dynamicWorkdir = r.Header.Get("X-Project-Root") // Try alternative
		if dynamicWorkdir != "" {
			source = "header"
		}
	}
	if dynamicWorkdir == "" {
		dynamicWorkdir = r.Header.Get("X-Working-Dir") // Try another alternative
		if dynamicWorkdir != "" {
			source = "header"
		}
	}

	// FALLBACK: Check system prompt for <env>Working directory: ...</env>
	if dynamicWorkdir == "" {
		dynamicWorkdir = extractWorkdirFromSystem(req.System)
		if dynamicWorkdir != "" {
			source = "system"
			slog.Info("Using workdir from system prompt env block", "workdir", dynamicWorkdir)
		}
	}

	// FINAL FALLBACK: Check session persistence
	if dynamicWorkdir == "" {
		if prevWorkdir != "" {
			dynamicWorkdir = prevWorkdir
			source = "session"
			slog.Info("Recovered workdir from session", "workdir", dynamicWorkdir, "session", conversationKey)
		}
	}
	// Fallback to config default if still empty
	if dynamicWorkdir == "" && h != nil && h.config != nil {
		if strings.TrimSpace(h.config.OrchidsLocalWorkdir) != "" {
			dynamicWorkdir = h.config.OrchidsLocalWorkdir
			source = "config"
			slog.Info("Using workdir from config", "workdir", dynamicWorkdir)
		}
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
	normalizedPrev := filepath.Clean(strings.TrimSpace(prevWorkdir))
	normalizedNext := filepath.Clean(strings.TrimSpace(dynamicWorkdir))
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

const (
	sessionMaxSize        = 1024
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
			delete(h.sessionLastAccess, key)
		}
	}
	h.sessionCleanupRun = now
}
