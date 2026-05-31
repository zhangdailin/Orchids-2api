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

func uniqueModelCandidates(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (h *Handler) resolveModelAliasForChannel(ctx context.Context, channel, modelID string) (string, *store.Model) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}

	candidates := resolveModelAliasCandidates(modelID)
	if strings.EqualFold(channel, "warp") {
		if mapped := warp.ResolveModelAlias(modelID); mapped != "" {
			candidates = append(resolveModelAliasCandidates(mapped), candidates...)
		}
	}
	candidates = uniqueModelCandidates(candidates...)

	var fallbackID string
	var fallbackModel *store.Model
	for _, cand := range candidates {
		m, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, cand)
		if err != nil || m == nil {
			continue
		}
		if m.Status.Enabled() {
			return cand, m
		}
		if fallbackModel == nil {
			fallbackID = cand
			fallbackModel = m
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
func (h *Handler) selectAccount(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, modelID ...string) (UpstreamClient, *store.Account, error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.selectAccountRecord(ctx, targetChannel, failedAccountIDs, firstString(modelID...))
		if err != nil {
			if channelRequired {
				return nil, nil, err
			}
			if h.client != nil {
				if _, ok := h.client.(*orchids.Client); ok && h.config != nil {
					h.client = orchids.New(h.config)
				}
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
		if _, ok := h.client.(*orchids.Client); ok && h.config != nil {
			h.client = orchids.New(h.config)
		}
		return h.client, nil, nil
	}
	return nil, nil, errors.New("no client configured")
}

func (h *Handler) selectAccountRecord(ctx context.Context, targetChannel string, failedAccountIDs []int64, modelID string) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	if !strings.EqualFold(strings.TrimSpace(targetChannel), "warp") {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
	}

	requestedModel := warp.ResolveModelAlias(modelID)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(modelID)
	}
	if requestedModel == "" || requestedModel == warp.DefaultModel() {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
	}

	choices, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
	if err != nil || choices == nil {
		return h.selectWarpAccountAvoidingUnavailable(ctx, targetChannel, failedAccountIDs, requestedModel)
	}
	if !h.warpEffectiveChoicesSupportModel(ctx, choices, requestedModel) {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (model %s is not available in the current Warp account pool)", targetChannel, requestedModel)
	}

	account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
		return warp.AccountSupportsModelForAccount(choices, acc, requestedModel) &&
			!warp.AccountModelTemporarilyUnavailable(ctx, h.loadBalancer.Store, acc.ID, requestedModel, time.Now())
	})
	if err == nil {
		return account, nil
	}

	return nil, err
}

func (h *Handler) warpEffectiveChoicesSupportModel(ctx context.Context, choices *warp.AccountModelChoices, modelID string) bool {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil || choices == nil || len(choices.Accounts) == 0 {
		return true
	}
	visible := h.visibleWarpModelSet(ctx)
	if visible == nil {
		return warp.ChoicesSupportModel(choices, modelID)
	}
	rawModelID := modelID
	resolvedModelID := warp.ResolveModelAlias(rawModelID)
	if resolvedModelID == "" {
		resolvedModelID = warp.NormalizeModelID(rawModelID)
	}
	if resolvedModelID == "" {
		return true
	}
	_, ok := visible[resolvedModelID]
	return ok
}

func (h *Handler) selectWarpAccountAvoidingUnavailable(ctx context.Context, targetChannel string, failedAccountIDs []int64, requestedModel string) (*store.Account, error) {
	account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
		return warp.AccountSupportsModelForAccount(nil, acc, requestedModel) &&
			!warp.AccountModelTemporarilyUnavailable(ctx, h.loadBalancer.Store, acc.ID, requestedModel, time.Now())
	})
	if err == nil {
		return account, nil
	}
	slog.Warn("Warp unavailable-aware account selection found no matching account; falling back to channel selection", "model", requestedModel, "error", err)
	return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
}

func firstString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func (h *Handler) acquireTrackedAccount(acc *store.Account) int64 {
	if acc == nil || acc.ID == 0 {
		return 0
	}
	if h != nil && h.connTracker != nil {
		h.connTracker.Acquire(acc.ID)
		return acc.ID
	}
	if h != nil && h.loadBalancer != nil {
		h.loadBalancer.AcquireConnection(acc.ID)
		return acc.ID
	}
	return 0
}

func (h *Handler) releaseTrackedAccount(accountID int64) {
	if accountID == 0 {
		return
	}
	if h != nil && h.connTracker != nil {
		h.connTracker.Release(accountID)
		return
	}
	if h != nil && h.loadBalancer != nil {
		h.loadBalancer.ReleaseConnection(accountID)
	}
}

func (h *Handler) validateModelAvailability(ctx context.Context, modelID, forcedChannel string) (*store.Model, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	var m *store.Model
	if forcedChannel != "" {
		_, m = h.resolveModelAliasForChannel(ctx, forcedChannel, modelID)
	} else {
		_, m = h.resolveModelAlias(ctx, modelID)
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
		if !sameModelChannel(mChannel, forcedChannel) {
			return nil, fmt.Errorf("model not found")
		}
	}
	return m, nil
}

func sameModelChannel(a, b string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.ReplaceAll(value, "_", "-")
		value = strings.ReplaceAll(value, " ", "-")
		if value == "" {
			return "orchids"
		}
		return value
	}
	return normalize(a) == normalize(b)
}

func (h *Handler) updateAccountStats(account *store.Account, inputTokens, outputTokens int) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	go func(accountID int64, inputTokens, outputTokens int) {
		usage := float64(inputTokens + outputTokens)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Count each completed request exactly once here. This avoids the old
		// pre-selection increment plus post-response stats update double-counting.
		if err := h.loadBalancer.Store.IncrementAccountStats(ctx, accountID, usage, 1); err != nil {
			slog.Error("Failed to update account stats", "account_id", accountID, "error", err)
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
	slog.Debug("Warp credits refunded", "category", category, "reason", reason)
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

func shouldRetryCurrentAccountWhenNoAlternative(category string) bool {
	switch strings.TrimSpace(category) {
	case "network", "timeout", "server", "model_unavailable", "unknown":
		return true
	default:
		return false
	}
}
