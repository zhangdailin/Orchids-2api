package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
	"orchids-api/internal/workbuddy"
)

func normalizeRequestedModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return modelID
}

func (h *Handler) resolveModelAlias(ctx context.Context, modelID string) (string, *store.Model) {
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

// ChannelForModel reports the channel a model id is registered under, or an
// empty string when the model is unknown. The unified /v1 routes use it to pick
// between the native and the bridged implementation.
//
// The request model hint is consulted first: the unified entry points publish
// the exact model string they are about to serve, and a Codex client asks about
// the *family* name ("gpt-5-6-sol") while the catalog only stores the
// effort-suffixed variants. Resolving through the hint keeps the answer
// identical to the one the request path will compute, instead of a second,
// subtly different lookup.
func (h *Handler) ChannelForModel(ctx context.Context, modelID string) string {
	channel, _ := h.LookupChannelForModel(ctx, modelID)
	return channel
}

// LookupChannelForModel is ChannelForModel with the store error preserved. A
// caller that must choose between two implementations needs to tell "this model
// belongs to another channel" from "the store could not be read"; collapsing
// both onto an empty channel is how a Redis hiccup turns into a wrong route.
func (h *Handler) LookupChannelForModel(ctx context.Context, modelID string) (string, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return "", nil
	}
	if hinted := strings.TrimSpace(middleware.RequestModelFromContext(ctx)); hinted != "" {
		modelID = hinted
	}
	modelID = normalizeRequestedModelID(modelID)
	if modelID == "" {
		return "", nil
	}
	m, err := h.loadBalancer.Store.GetModelByModelID(ctx, modelID)
	if err == nil && m != nil {
		return strings.TrimSpace(m.Channel), nil
	}
	if err != nil && !isModelMissingError(err) {
		return "", err
	}
	// A family name is not a catalog row. Fall back to the effort variant the
	// request path would have selected, so "/v1/models/gpt-5-6-sol" and
	// "/v1/chat/completions" agree on the channel.
	variant := h.resolveEffortModelVariant(ctx, modelID, "", "")
	if variant == "" || variant == modelID {
		return "", nil
	}
	m, err = h.loadBalancer.Store.GetModelByModelID(ctx, variant)
	if err != nil {
		if isModelMissingError(err) {
			return "", nil
		}
		return "", err
	}
	if m == nil {
		return "", nil
	}
	return strings.TrimSpace(m.Channel), nil
}

// isModelMissingError reports whether a model lookup failed because the row does
// not exist. The stores signal that both ways — ErrNoRows and a plain
// "model not found" — and a caller that separates "unknown model" from "store
// unavailable" has to accept both, or every miss looks like an outage.
func isModelMissingError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrNoRows) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "model not found")
}

// ModelChannel reports the channel a request should be served by. It is the one
// place that decides between "the path already names a channel" and "the model
// decides". Every entry point that used to call channelFromPath on a body that
// carries a model must call this instead: on the unified prefix the path carries
// no channel at all, so a path-only answer silently degrades to the generic
// code path (which is how a Warp token count came back with the wrong profile).
func (h *Handler) ModelChannel(r *http.Request, modelID string) string {
	if r != nil {
		if channel := channelFromPath(r.URL.Path); channel != "" {
			return channel
		}
	}
	if h == nil {
		return ""
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	if modelID == "" {
		if r != nil {
			modelID = middleware.RequestModelFromContext(ctx)
		}
	}
	return h.ChannelForModel(ctx, modelID)
}

// requestReasoningEffort returns the effort a client asked for, from whichever
// dialect it used: OpenAI's reasoning_effort or Anthropic's
// output_config.effort / thinking. A thinking budget without an explicit effort
// maps onto the same coarse levels so an effort-suffixed catalog can still be
// resolved.
func requestReasoningEffort(req ClaudeRequest) string {
	if effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort)); effort != "" {
		return effort
	}
	for _, config := range []map[string]interface{}{req.OutputConfig, req.Thinking} {
		value, ok := config["effort"].(string)
		if !ok {
			continue
		}
		if effort := strings.ToLower(strings.TrimSpace(value)); effort != "" {
			return effort
		}
	}
	if req.Thinking == nil {
		return ""
	}
	budget, ok := looseNumber(req.Thinking["budget_tokens"])
	if !ok || budget <= 0 {
		return ""
	}
	switch {
	case budget < 4096:
		return "low"
	case budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// looseNumber accepts the numeric shapes JSON decoding produces.
func looseNumber(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// effortVariantOrder is the fallback order tried when a client asks for a model
// family by its bare name and the catalog only exposes effort-suffixed
// variants. Warp publishes models as "<family>-<effort>" (gpt-5-6-sol-low),
// while clients such as Codex send the family name plus reasoning_effort.
var effortVariantOrder = []string{"medium", "high", "low", "xhigh", "max"}

// resolveEffortModelVariant maps a bare model name onto the catalog entry that
// actually exists. An exact catalog hit always wins; otherwise the requested
// reasoning_effort is tried as a suffix and then the default effort order.
// Without this, "gpt-5-6-sol" is rejected as "model not found" even though the
// family is available and the client stated which effort it wants.
//
// A model that already carries an effort suffix is never suffixed again: the
// old code would have tried "gpt-5-6-sol-low-low" and then fallen through to
// "-medium", silently serving an effort the client never asked for.
func (h *Handler) resolveEffortModelVariant(ctx context.Context, modelID, effort, forcedChannel string) string {
	modelID = normalizeRequestedModelID(modelID)
	if modelID == "" || h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return modelID
	}
	lookup := func(id string) *store.Model {
		if forcedChannel != "" {
			_, m := h.resolveModelAliasForChannel(ctx, forcedChannel, id)
			return m
		}
		_, m := h.resolveModelAlias(ctx, id)
		return m
	}
	if m := lookup(modelID); m != nil {
		return modelID
	}
	// "gpt-5-6-sol-low" is final: it is an exact catalog entry under a name that
	// happens to end in an effort word, and appending another suffix can only
	// produce a wrong model.
	if _, level := splitEffortVariantSuffix(modelID); level != "" {
		return modelID
	}

	effort = strings.ToLower(strings.TrimSpace(effort))
	candidates := make([]string, 0, len(effortVariantOrder)+1)
	if effort != "" {
		candidates = append(candidates, modelID+"-"+effort)
	}
	for _, suffix := range effortVariantOrder {
		candidates = append(candidates, modelID+"-"+suffix)
	}
	for _, candidate := range candidates {
		if m := lookup(candidate); m != nil && m.Status.Enabled() {
			slog.Info("Resolved bare model to an effort variant",
				"model", modelID, "resolved", candidate, "reasoning_effort", effort, "channel", forcedChannel)
			return candidate
		}
	}
	return modelID
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
	ModelID            string
	PreferredAccountID int64
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

// honorsModelCooldown reports whether a channel's selection consults the
// per-model cooldown its own verdicts write. Qoder scopes agent/model windows;
// WorkBuddy may scope a plan refusal while accounts with a truly exhausted
// package remain account-wide parked.
func honorsModelCooldown(channel string) bool {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "qoder", "workbuddy":
		return true
	default:
		return false
	}
}

func (h *Handler) isCurrentFreeModel(ctx context.Context, channel, modelID string) bool {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return false
	}
	model, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, strings.ToLower(strings.TrimSpace(modelID)))
	return err == nil && model != nil && strings.EqualFold(strings.TrimSpace(model.BillingTier), "free")
}

func (h *Handler) selectAccountRecordWithOptions(ctx context.Context, targetChannel string, failedAccountIDs []int64, opts accountSelectionOptions) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	if !strings.EqualFold(strings.TrimSpace(targetChannel), "warp") {
		model := strings.TrimSpace(opts.ModelID)
		channel := strings.ToLower(strings.TrimSpace(targetChannel))
		needsFilter := model != "" && (honorsModelCooldown(channel) || channel == "puter" || channel == "qoder" || channel == "workbuddy")
		if needsFilter {
			return h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) bool {
				if honorsModelCooldown(channel) && store.ModelCooldownRemaining(acc, model, time.Now()) != 0 {
					return false
				}
				switch strings.TrimSpace(acc.StatusCode) {
				case store.AccountStatusPuterQuotaExhausted, "402":
					if channel == "puter" {
						return h.isCurrentFreeModel(ctx, "puter", model)
					}
					if channel == "qoder" {
						return qoder.IsFreeModel(acc.QoderModelIDs, model) && h.isCurrentFreeModel(ctx, "qoder", model)
					}
					if channel == "workbuddy" {
						return workbuddy.IsFreeModelInCatalog(acc.WorkBuddyModelIDs, model)
					}
				case store.AccountStatusQoderQuotaExhausted:
					return channel == "qoder" && qoder.IsFreeModel(acc.QoderModelIDs, model) && h.isCurrentFreeModel(ctx, "qoder", model)
				case store.AccountStatusWorkBuddyQuotaExhausted:
					return channel == "workbuddy" && workbuddy.IsFreeModelInCatalog(acc.WorkBuddyModelIDs, model)
				default:
					return true
				}
				return false
			})
		}
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
	}

	requestedModel := normalizeRequestedModelID(opts.ModelID)
	warpFilter := func(acc *store.Account) bool {
		if opts.PreferredAccountID != 0 && acc.ID != opts.PreferredAccountID {
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
		return warpFilter(acc) && warp.AccountSupportsModelForRouting(choices, acc, requestedModel)
	})
	if err == nil {
		return account, nil
	}
	return nil, err
}

func (h *Handler) selectWarpAccountWithFilter(ctx context.Context, failedAccountIDs []int64, targetChannel string, opts accountSelectionOptions, filter func(*store.Account) bool) (*store.Account, error) {
	account, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, filter)
	if err == nil {
		return account, nil
	}
	// Account pricing is not used as a routing restriction: the upstream response
	// is authoritative for any feature entitlement.
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

// warpRequestFeatures is everything the request builder needs from the account's
// own model discovery: the agent defaults and the base model's input window.
// They resolve together because both come from the same stored snapshot, and a
// second read would only be another way for the two to disagree.
type warpRequestFeatures struct {
	Config        warp.AccountFeatureConfig
	ContextWindow uint32
}

func (h *Handler) resolveWarpRequestFeatures(ctx context.Context, acc *store.Account, requestedModel string) warpRequestFeatures {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
		return warpRequestFeatures{}
	}
	var choices *warp.AccountModelChoices
	if h != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
		loaded, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store)
		if err == nil {
			choices = loaded
		}
	}
	return warpRequestFeatures{
		Config:        warp.EffectiveAccountFeatureConfig(acc, choices, requestedModel),
		ContextWindow: warp.ModelContextWindowLimitFor(choices, requestedModel),
	}
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

		discovery := warp.AccountModelDiscovery{
			AccountID:     account.ID,
			Source:        source,
			Choices:       choices,
			FeatureConfig: warp.AccountFeatureConfigFromChoices(features),
		}
		if err := warp.UpsertAccountModelDiscoveries(ctx, h.loadBalancer.Store, discovery); err != nil {
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

type accountStatsDelta struct {
	usage float64
	count int64
}

func (h *Handler) updateAccountStats(account *store.Account, inputTokens, outputTokens int) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	h.statsOnce.Do(func() {
		h.statsPending = make(map[int64]accountStatsDelta)
		h.statsWake = make(chan struct{}, 1)
		go h.runAccountStatsWriter()
	})
	h.statsMu.Lock()
	delta := h.statsPending[account.ID]
	delta.usage += float64(inputTokens + outputTokens)
	delta.count++
	h.statsPending[account.ID] = delta
	h.statsMu.Unlock()
	select {
	case h.statsWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runAccountStatsWriter() {
	for range h.statsWake {
		for {
			h.statsMu.Lock()
			var accountID int64
			var delta accountStatsDelta
			for id, pending := range h.statsPending {
				accountID, delta = id, pending
				delete(h.statsPending, id)
				break
			}
			h.statsMu.Unlock()
			if accountID == 0 {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.loadBalancer.Store.IncrementAccountStats(ctx, accountID, delta.usage, delta.count)
			cancel()
			if err != nil {
				slog.Error("Failed to update account stats", "account_id", accountID, "error", err)
			}
		}
	}
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
