package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func normalizeRequestedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if isWarpVirtualModel(modelID) {
		return upstreamWarpModelID(modelID)
	}
	return modelID
}

func (h *Handler) resolveModelAlias(ctx context.Context, modelID string) (string, *store.Model) {
	if m := warpVirtualModelRecord(modelID); m != nil {
		return m.ModelID, m
	}
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return modelID, nil
	}
	if m, err := h.loadBalancer.Store.GetModelByModelID(ctx, candidate); err == nil && m != nil {
		return candidate, m
	}
	return modelID, nil
}

func (h *Handler) resolveModelAliasForChannel(ctx context.Context, channel, modelID string) (string, *store.Model) {
	if strings.EqualFold(strings.TrimSpace(channel), "warp") {
		if m := warpVirtualModelRecord(modelID); m != nil {
			return m.ModelID, m
		}
	}
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID, nil
	}
	candidate := normalizeRequestedModelID(modelID)
	if candidate == "" {
		return modelID, nil
	}
	if m, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, candidate); err == nil && m != nil {
		return candidate, m
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

type accountSelectionOptions struct {
	ModelID               string
	RequireWarpCloudAgent bool
	PreferWarpFreeAccount bool
	PreferredAccountID    int64
}

// acquireAccountSelection is the form the request path uses: it returns the
// release handle for the account client, so a client evicted mid-request is
// closed only after that request finishes.
func (h *Handler) acquireAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.selectAccountRecordWithOptions(ctx, targetChannel, failedAccountIDs, opts)
		if err != nil {
			if channelRequired {
				return nil, nil, func() {}, err
			}
			if h.client != nil {
				slog.Debug("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
				return h.client, nil, func() {}, nil
			}
			return nil, nil, func() {}, err
		}
		client, release := h.acquireAccountClient(account)
		if client == nil {
			return nil, nil, func() {}, errors.New("no client configured")
		}
		return client, account, release, nil
	} else if h.client != nil {
		return h.client, nil, func() {}, nil
	}
	return nil, nil, func() {}, errors.New("no client configured")
}

// acquireReservedAccountSelection closes the check-then-increment race between
// account selection and connection tracking. A candidate is not returned until
// its per-account slot has been atomically reserved; if another request wins the
// last slot, selection continues with the remaining accounts.
func (h *Handler) acquireReservedAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), int64, error) {
	excluded := append([]int64(nil), failedAccountIDs...)
	full := make(map[int64]struct{})
	// Account leases are held for the complete upstream request. When another
	// request is just finishing, an immediate second selection can observe all
	// accounts at their hard limit and turn a transient race into a 503. Give
	// releases a bounded, cancellable window to become visible before failing.
	// The two-second window matches the busy retry used by Qoder and is still
	// short enough that a genuinely saturated Puter pool fails promptly.
	const (
		reservationRetries    = 8
		reservationRetryDelay = 250 * time.Millisecond
	)
	reservationAttempt := 0
	for {
		client, account, release, err := h.acquireAccountSelection(ctx, targetChannel, channelRequired, excluded, opts)
		if err != nil {
			if reservationAttempt < reservationRetries && strings.Contains(err.Error(), "all matching accounts are at their concurrency limit") {
				reservationAttempt++
				// Rebuild the exclusion set from caller-supplied failures. Entries in
				// full only represent accounts that lost a reservation race and may
				// be available again on the next pass.
				excluded = append([]int64(nil), failedAccountIDs...)
				clear(full)
				timer := time.NewTimer(reservationRetryDelay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return nil, nil, func() {}, 0, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if len(full) > 0 {
				return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
			}
			return nil, nil, func() {}, 0, err
		}
		accountID, acquired := h.tryAcquireTrackedAccount(account)
		if acquired {
			return client, account, release, accountID, nil
		}
		release()
		if account == nil || account.ID == 0 {
			return nil, nil, func() {}, 0, fmt.Errorf("failed to reserve an account concurrency slot")
		}
		if _, seen := full[account.ID]; seen {
			return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
		}
		full[account.ID] = struct{}{}
		excluded = append(excluded, account.ID)
	}
}

func (h *Handler) selectAccountRecordWithOptions(ctx context.Context, targetChannel string, failedAccountIDs []int64, opts accountSelectionOptions) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	if !strings.EqualFold(strings.TrimSpace(targetChannel), "warp") {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
	}

	requestedModel := normalizeRequestedModelID(opts.ModelID)
	warpFilter := func(acc *store.Account) bool {
		if opts.PreferredAccountID != 0 && acc.ID != opts.PreferredAccountID {
			return false
		}
		if opts.RequireWarpCloudAgent && !warp.AccountSupportsCloudAgent(acc) {
			return false
		}
		return true
	}
	if requestedModel == "" || requestedModel == warp.DefaultModel() {
		return h.selectWarpAccountWithFilter(ctx, failedAccountIDs, targetChannel, opts, warpFilter)
	}

	choices, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
	if err != nil || choices == nil {
		return h.selectWarpAccountWithFilter(ctx, failedAccountIDs, targetChannel, opts, warpFilter)
	}
	if !h.warpEffectiveChoicesSupportModel(ctx, choices, requestedModel) {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (model %s is not available in the current Warp account pool)", targetChannel, requestedModel)
	}

	account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
		return warpFilter(acc) && warp.AccountSupportsModelForAccount(choices, acc, requestedModel)
	})
	if err == nil {
		return account, nil
	}
	if opts.RequireWarpCloudAgent {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (cloud agent requires a non-free Warp account)", targetChannel)
	}

	return nil, err
}

func (h *Handler) selectWarpAccountWithFilter(ctx context.Context, failedAccountIDs []int64, targetChannel string, opts accountSelectionOptions, filter func(*store.Account) bool) (*store.Account, error) {
	if opts.PreferWarpFreeAccount && opts.PreferredAccountID == 0 {
		account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
			return filter(acc) && warp.AccountFreeOnly(acc)
		})
		if err == nil {
			return account, nil
		}
	}
	account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, filter)
	if err == nil {
		return account, nil
	}
	if opts.RequireWarpCloudAgent {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (cloud agent requires a non-free Warp account)", targetChannel)
	}
	return nil, err
}

func (h *Handler) warpEffectiveChoicesSupportModel(ctx context.Context, choices *warp.AccountModelChoices, modelID string) bool {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil || choices == nil || len(choices.Accounts) == 0 {
		return true
	}
	resolvedModelID := normalizeRequestedModelID(modelID)
	if resolvedModelID == "" {
		return true
	}
	visible := h.visibleWarpModelSet(ctx)
	if visible == nil {
		for _, models := range choices.Accounts {
			for _, cachedModel := range models {
				if normalizeRequestedModelID(cachedModel) == resolvedModelID {
					return true
				}
			}
		}
		return false
	}
	_, ok := visible[resolvedModelID]
	return ok
}

func (h *Handler) resolveWarpFeatureConfig(ctx context.Context, acc *store.Account, requestedModel string) warp.AccountFeatureConfig {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
		return warp.AccountFeatureConfig{}
	}
	var choices *warp.AccountModelChoices
	if h != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
		loaded, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
		if err == nil {
			choices = loaded
		}
	}
	return warp.EffectiveAccountFeatureConfig(acc, choices, requestedModel)
}

// refreshWarpModelConfigAsync consumes Warp's stale-config signal without
// delaying the completed user request. Each account has at most one refresh in
// flight; a successful discovery atomically replaces that account's advisory
// routing choices and feature defaults.
func (h *Handler) refreshWarpModelConfigAsync(acc *store.Account) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil || acc == nil || acc.ID == 0 || !strings.EqualFold(acc.AccountType, "warp") {
		return
	}
	if _, loaded := h.warpModelRefreshes.LoadOrStore(acc.ID, struct{}{}); loaded {
		return
	}
	account := *acc
	go func() {
		defer h.warpModelRefreshes.Delete(account.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		client := warp.NewFromAccount(&account, h.configSnapshot())
		defer client.Close()
		features, source, err := client.FetchDiscoveredFeatureModelChoices(ctx)
		if err != nil {
			slog.Warn("Warp model config refresh failed", "account_id", account.ID, "error", err)
			return
		}
		choices := warp.AgentModeModelChoices(features)
		if len(choices) == 0 {
			slog.Warn("Warp model config refresh returned no enabled models", "account_id", account.ID)
			return
		}

		existing, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
		if err != nil {
			slog.Warn("Warp model config cache load failed", "account_id", account.ID, "error", err)
			return
		}
		if existing == nil {
			existing = &warp.AccountModelChoices{}
		}
		if existing.Accounts == nil {
			existing.Accounts = make(map[string][]string)
		}
		if existing.Sources == nil {
			existing.Sources = make(map[string]string)
		}
		if existing.FeatureConfigs == nil {
			existing.FeatureConfigs = make(map[string]warp.AccountFeatureConfig)
		}
		models := make([]string, 0, len(choices))
		for _, choice := range choices {
			models = append(models, choice.ID)
		}
		key := strconv.FormatInt(account.ID, 10)
		existing.Accounts[key] = models
		existing.Sources[key] = source
		if config := warp.AccountFeatureConfigFromChoices(features); !config.IsEmpty() {
			existing.FeatureConfigs[key] = config
		}
		if err := warp.SaveAccountModelChoices(ctx, h.loadBalancer.Store, existing); err != nil {
			slog.Warn("Warp model config cache save failed", "account_id", account.ID, "error", err)
			return
		}
		slog.Info("Warp model config refreshed", "account_id", account.ID, "source", source)
	}()
}

func effectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	return loadbalancer.EffectiveAccountConcurrencyLimit(acc)
}

func (h *Handler) tryAcquireTrackedAccount(acc *store.Account) (int64, bool) {
	if acc == nil || acc.ID == 0 {
		return 0, true
	}
	limit := effectiveAccountConcurrencyLimit(acc)
	if h != nil && h.connTracker != nil {
		if limited, ok := h.connTracker.(loadbalancer.LimitedConnTracker); ok && limit > 0 {
			if !limited.TryAcquire(acc.ID, limit) {
				return 0, false
			}
			return acc.ID, true
		}
		if limit > 0 && h.connTracker.GetCount(acc.ID) >= limit {
			return 0, false
		}
		h.connTracker.Acquire(acc.ID)
		return acc.ID, true
	}
	if h != nil && h.loadBalancer != nil {
		// The load balancer's built-in tracker is only a compatibility fallback;
		// production wires a shared tracker directly onto the handler.
		h.loadBalancer.AcquireConnection(acc.ID)
		return acc.ID, true
	}
	return 0, true
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
	if strings.TrimSpace(forcedChannel) == "" || strings.EqualFold(strings.TrimSpace(forcedChannel), "warp") {
		if m := warpVirtualModelRecord(modelID); m != nil {
			return m, nil
		}
	}
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
			mChannel = ""
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
			return ""
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

func (h *Handler) syncWarpState(account *store.Account, client UpstreamClient) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}

	var changed bool
	if strings.EqualFold(account.AccountType, "warp") {
		if warpClient, ok := client.(*warp.Client); ok {
			changed = warpClient.SyncAccountStateTo(account)
		}
	}

	if changed {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.loadBalancer.Store.UpdateAccount(ctx, account); err != nil {
			slog.Warn("同步账号令牌失败", "account", account.Name, "type", account.AccountType, "error", err)
		}
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

func shouldRetryCurrentAccountWhenNoAlternative(category string) bool {
	switch strings.TrimSpace(category) {
	case "network", "timeout", "server", "model_unavailable", "unknown":
		return true
	default:
		return false
	}
}
