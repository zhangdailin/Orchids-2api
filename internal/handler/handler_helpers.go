package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/orchids"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

var modelVersionHyphenAlias = regexp.MustCompile(`-(\d{1,2})-(\d{1,2})`)
var modelVersionDotAlias = regexp.MustCompile(`-(\d{1,2})\.(\d{1,2})`)

func resolveModelAliasCandidates(modelID string) []string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return nil
	}
	seen := map[string]struct{}{}
	add := func(v string, out *[]string) {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		*out = append(*out, v)
	}

	out := make([]string, 0, 3)
	add(modelID, &out)
	add(modelVersionHyphenAlias.ReplaceAllString(modelID, "-$1.$2"), &out)
	add(modelVersionDotAlias.ReplaceAllString(modelID, "-$1-$2"), &out)
	return out
}

func (h *Handler) resolveModelAlias(ctx context.Context, modelID string) (string, *store.Model) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}
	candidates := resolveModelAliasCandidates(modelID)
	if len(candidates) == 0 {
		return modelID, nil
	}
	var fallbackID string
	var fallbackModel *store.Model
	for _, cand := range candidates {
		if m, err := h.loadBalancer.Store.GetModelByModelID(ctx, cand); err == nil && m != nil {
			if m.Status.Enabled() {
				return cand, m
			}
			if fallbackModel == nil {
				fallbackID = cand
				fallbackModel = m
			}
		}
	}
	if fallbackModel != nil {
		return fallbackID, fallbackModel
	}
	return modelID, nil
}

// resolveWorkdir determines the working directory from headers, system prompt, or session.
// 返回当前 workdir、上一轮 workdir、以及是否发生变更。
func (h *Handler) resolveWorkdir(r *http.Request, req ClaudeRequest, conversationKey string) (string, string, bool) {
	prevWorkdir := ""
	if conversationKey != "" {
		prevWorkdir, _ = h.sessionStore.GetWorkdir(r.Context(), conversationKey)
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
		slog.Debug("Recovered workdir from session", "workdir", dynamicWorkdir, "session", conversationKey)
	}

	// Persist for future turns in this session
	if dynamicWorkdir != "" && conversationKey != "" {
		h.sessionStore.SetWorkdir(r.Context(), conversationKey, dynamicWorkdir)
		h.sessionStore.Touch(r.Context(), conversationKey)
	}

	if dynamicWorkdir != "" {
		slog.Debug("Using dynamic workdir", "workdir", dynamicWorkdir, "source", source)
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
func (h *Handler) selectAccount(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64) (UpstreamClient, *store.Account, error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.loadBalancer.GetNextAccountExcludingByChannel(ctx, failedAccountIDs, targetChannel)
		if err != nil {
			if channelRequired {
				return nil, nil, err
			}
			if h.client != nil {
				slog.Debug("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
				return h.client, nil, nil
			}
			return nil, nil, err
		}
		client := h.getOrCreateAccountClient(account)
		if client == nil {
			return nil, nil, errors.New("no client configured")
		}
		return client, account, nil
	} else if h.client != nil {
		return h.client, nil, nil
	}
	return nil, nil, errors.New("no client configured")
}

func (h *Handler) validateModelAvailability(ctx context.Context, modelID, forcedChannel string) (*store.Model, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	resolvedModelID, m := h.resolveModelAlias(ctx, modelID)
	if strings.EqualFold(forcedChannel, "warp") {
		if mapped := warp.ResolveModelAlias(resolvedModelID); mapped != "" {
			resolvedModelID = mapped
			if m == nil || !strings.EqualFold(strings.TrimSpace(m.ModelID), resolvedModelID) {
				resolved, err := h.loadBalancer.Store.GetModelByModelID(ctx, resolvedModelID)
				if err == nil {
					m = resolved
				}
			}
		}
	}
	if m == nil {
		return nil, fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return nil, fmt.Errorf("model not available")
	}
	if forcedChannel != "" {
		mChannel := strings.TrimSpace(m.Channel)
		if mChannel == "" {
			mChannel = "orchids"
		}
		if !strings.EqualFold(mChannel, forcedChannel) {
			return nil, fmt.Errorf("model not found")
		}
	}
	return m, nil
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
	} else if _, ok := client.(*orchids.Client); ok {
		// Orchids 账号：通过快照比较检测 forceRefreshToken 是否更新了账号信息
		changed = account.SyncState(snapshot)
	}

	if changed {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.loadBalancer.Store.UpdateAccount(ctx, account); err != nil {
			slog.Warn("同步账号令牌失败", "account", account.Name, "type", account.AccountType, "error", err)
		}
	}
}

type creditRefundClient interface {
	RefundCredits(ctx context.Context, reason string) error
}

func shouldRefundWarpCredits(category string) bool {
	switch strings.TrimSpace(category) {
	case "canceled", "timeout", "network", "server", "unknown":
		return true
	default:
		return false
	}
}

func refundReasonForWarpCategory(category string) string {
	switch strings.TrimSpace(category) {
	case "canceled":
		return "request_canceled"
	case "timeout":
		return "request_timeout"
	case "network":
		return "network_error"
	case "server":
		return "server_error"
	default:
		return "upstream_error"
	}
}

func (h *Handler) refundWarpCredits(client UpstreamClient, category string) {
	if !shouldRefundWarpCredits(category) {
		return
	}

	refundable, ok := client.(creditRefundClient)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	reason := refundReasonForWarpCategory(category)
	if err := refundable.RefundCredits(ctx, reason); err != nil {
		slog.Warn("Warp refund credits failed", "category", category, "reason", reason, "error", err)
		return
	}
	slog.Info("Warp credits refunded", "category", category, "reason", reason)
}

// upstreamErrorClass is a local alias for the centralized type.
type upstreamErrorClass = apperrors.UpstreamErrorClass

// classifyUpstreamError delegates to the centralized errors package.
func classifyUpstreamError(errStr string) upstreamErrorClass {
	return apperrors.ClassifyUpstreamError(errStr)
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
