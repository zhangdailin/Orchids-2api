package grok

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

const grokModelValidationCacheTTL = 3 * time.Second

type Handler struct {
	base         *handler.BaseHandler
	cfg          *config.Config
	lb           *loadbalancer.LoadBalancer
	client       *Client
	connTracker  loadbalancer.ConnTracker
	modelCacheMu sync.RWMutex
	modelCache   map[string]time.Time
}

type chatAccountSession struct {
	acc            *store.Account
	token          string
	poolCandidates []string
	release        func()
}

type imageEditUploadInput struct {
	mime string
	data []byte
}

type imageEditReference struct {
	fileID     string
	contentURL string
}

func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	return &Handler{
		base:        handler.NewBaseHandler(lb),
		cfg:         cfg,
		lb:          lb,
		client:      New(cfg),
		connTracker: loadbalancer.NewMemoryConnTracker(),
		modelCache:  make(map[string]time.Time),
	}
}

func (h *Handler) currentClient() *Client {
	if h == nil {
		return nil
	}
	if h.cfg != nil {
		return New(h.cfg)
	}
	return h.client
}

func (h *Handler) isModelValidationCached(modelID string) bool {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return false
	}
	h.modelCacheMu.RLock()
	expiresAt, ok := h.modelCache[modelID]
	h.modelCacheMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().Before(expiresAt) {
		return true
	}
	h.modelCacheMu.Lock()
	if staleAt, ok := h.modelCache[modelID]; ok && !time.Now().Before(staleAt) {
		delete(h.modelCache, modelID)
	}
	h.modelCacheMu.Unlock()
	return false
}

func (h *Handler) cacheValidatedModel(modelID string) {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return
	}
	h.modelCacheMu.Lock()
	h.modelCache[modelID] = time.Now().Add(grokModelValidationCacheTTL)
	h.modelCacheMu.Unlock()
}

func (h *Handler) selectAccount(ctx context.Context) (*store.Account, string, error) {
	if h.lb == nil {
		return nil, "", fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTracker(ctx, nil, "grok", h.connTracker)
	if err != nil {
		return nil, "", err
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	if NormalizeSSOToken(raw) == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, raw, nil
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
	if IsDeprecatedModelID(id) {
		return fmt.Errorf("model not found")
	}
	if h.isModelValidationCached(id) {
		return nil
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		if !modelpolicy.IsPublicGrokModelID(id) {
			return fmt.Errorf("model not found")
		}
		h.cacheValidatedModel(id)
		return nil
	}

	m, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || m == nil {
		rawID := strings.ToLower(strings.TrimSpace(modelID))
		if rawID != "" && rawID != id {
			m, err = h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", rawID)
		}
	}
	if err != nil || m == nil {
		return fmt.Errorf("model not found")
	}
	if !modelpolicy.IsVisibleGrokModel(id, m.Verified) {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	channel := strings.TrimSpace(m.Channel)
	if channel == "" {
		channel = "grok"
	}
	if !strings.EqualFold(channel, "grok") {
		return fmt.Errorf("model not found")
	}
	h.cacheValidatedModel(id)
	return nil
}

func modelNotFoundMessage(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "The model does not exist or you do not have access to it."
	}
	return fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID)
}

func modelValidationMessage(modelID string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if strings.EqualFold(msg, "model not found") {
		return modelNotFoundMessage(modelID)
	}
	return msg
}

func (h *Handler) trackAccount(acc *store.Account) func() {
	if h == nil || h.connTracker == nil || acc == nil || acc.ID == 0 {
		return h.base.TrackAccount(acc)
	}
	h.connTracker.Acquire(acc.ID)
	return func() {
		h.connTracker.Release(acc.ID)
	}
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	h.base.MarkAccountStatus(ctx, acc, err)
}

func (h *Handler) openChatAccountSessionForModel(ctx context.Context, spec ModelSpec) (*chatAccountSession, error) {
	return h.openChatAccountSessionForModelExcluding(ctx, nil, spec)
}

func (h *Handler) openChatAccountSessionForModelExcluding(ctx context.Context, excludeIDs []int64, spec ModelSpec) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcludingWithPools(ctx, excludeIDs, spec.PoolCandidates())
}

func (h *Handler) openChatAccountSessionForImagineLite(ctx context.Context, excludeIDs []int64, spec ModelSpec) (*chatAccountSession, error) {
	spec.Tier = grokTierLite
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, spec.PoolCandidates(), func(acc *store.Account) bool {
		return grokAccountPool(acc) != "basic"
	})
}

func (h *Handler) openChatAccountSessionExcludingWithPools(ctx context.Context, excludeIDs []int64, poolCandidates []string) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, poolCandidates, nil)
}

func (h *Handler) openChatAccountSessionExcludingWithPoolsAndFilter(ctx context.Context, excludeIDs []int64, poolCandidates []string, extraFilter func(*store.Account) bool) (*chatAccountSession, error) {
	if h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	var (
		acc     *store.Account
		err     error
		lastErr error
	)
	candidates := normalizeGrokPoolCandidates(poolCandidates)
	if len(candidates) == 0 {
		acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTracker, extraFilter)
		if err != nil {
			return nil, err
		}
	} else {
		for _, pool := range candidates {
			wantPool := pool
			acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTracker, func(acc *store.Account) bool {
				return strings.EqualFold(grokAccountPool(acc), wantPool) && (extraFilter == nil || extraFilter(acc))
			})
			if err == nil && acc != nil {
				break
			}
			if err != nil {
				if lastErr == nil || strings.Contains(err.Error(), "rate-limited or cooling down") {
					lastErr = err
				}
			}
		}
		if acc == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("no enabled grok accounts available for requested pools: %s", strings.Join(candidates, ","))
		}
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	if NormalizeSSOToken(raw) == "" {
		return nil, fmt.Errorf("grok account token is empty")
	}
	return &chatAccountSession{
		acc:            acc,
		token:          raw,
		poolCandidates: candidates,
		release:        h.trackAccount(acc),
	}, nil
}

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// doSingleAccountRequest calls the Grok API once on a single account without retry switching.
func (h *Handler) doSingleAccountRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload map[string]interface{},
	shouldMarkStatus grokAccountStatusPolicy,
	callAPI func(*Client, context.Context, string, map[string]interface{}) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	resp, err := callAPI(client, ctx, sess.token, payload)
	if err != nil {
		if shouldMarkStatus == nil || shouldMarkStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		return nil, err
	}
	return resp, nil
}

type grokAccountStatusPolicy func(error) bool

func markAllGrokAccountStatuses(err error) bool {
	return err != nil
}

func skipExternalAttachmentFetchGrokAccountStatus(err error) bool {
	if err == nil {
		return false
	}
	return !strings.Contains(strings.ToLower(err.Error()), "fetch url status=")
}

// doChatWithAutoSwitchRebuild calls doChat with automatic account switching and payload rebuild on failure.
func (h *Handler) doChatWithAutoSwitchRebuild(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]interface{},
	rebuild func(token string) (map[string]interface{}, error),
) (*http.Response, error) {
	return h.doAutoSwitchRequest(ctx, sess, payload, rebuild, markAllGrokAccountStatuses, (*Client).doChat)
}

// doAutoSwitchRequest calls the Grok API with automatic account switching on failure.
func (h *Handler) doAutoSwitchRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]interface{},
	rebuild func(token string) (map[string]interface{}, error),
	shouldMarkStatus grokAccountStatusPolicy,
	callAPI func(*Client, context.Context, string, map[string]interface{}) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	if payload == nil {
		return nil, fmt.Errorf("empty payload")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	maxAttempts := 2
	if h != nil && h.cfg != nil && h.cfg.AccountSwitchCount > 0 {
		maxAttempts = h.cfg.AccountSwitchCount
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	used := make([]int64, 0, maxAttempts)
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if sess.acc != nil && sess.acc.ID != 0 {
			used = append(used, sess.acc.ID)
		}
		resp, err := callAPI(client, ctx, sess.token, *payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if shouldMarkStatus == nil || shouldMarkStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		if !shouldSwitchGrokAccount(err) || attempt == maxAttempts-1 {
			return nil, err
		}

		sess.Close()
		next, err2 := h.openChatAccountSessionExcludingWithPools(ctx, used, sess.poolCandidates)
		if err2 != nil {
			return nil, err
		}
		sess.acc = next.acc
		sess.token = next.token
		sess.poolCandidates = next.poolCandidates
		sess.release = next.release

		if rebuild != nil {
			newPayload, rbErr := rebuild(sess.token)
			if rbErr != nil {
				return nil, rbErr
			}
			*payload = newPayload
		}
	}
	return nil, lastErr
}

func isGrokAntiBotError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "anti-bot") ||
		strings.Contains(lower, "antibot") ||
		strings.Contains(lower, "request rejected by anti-bot rules")
}

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	status := classifyAccountStatusFromError(err.Error())
	if status == "403" || status == "429" {
		return true
	}
	if upstreamStatus := parseUpstreamStatus(err); upstreamStatus == http.StatusBadGateway ||
		upstreamStatus == http.StatusServiceUnavailable ||
		upstreamStatus == http.StatusGatewayTimeout ||
		upstreamStatus == http.StatusInternalServerError {
		return true
	}

	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "timeout"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "broken pipe"),
		strings.HasSuffix(lower, ": eof"),
		lower == "eof":
		return true
	default:
		return false
	}
}

func upstreamHTTPResponseStatus(err error) int {
	switch parseUpstreamStatus(err) {
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests
	case http.StatusForbidden:
		return http.StatusForbidden
	case http.StatusUnauthorized:
		return http.StatusUnauthorized
	case http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable
	case http.StatusGatewayTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	accCopy := *acc
	headers = headers.Clone()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.lb.Store.IncrementAccountStats(ctx, accCopy.ID, 0, 1); err != nil {
			slog.Warn("grok usage touch failed", "account_id", accCopy.ID, "error", err)
		}

		info := parseRateLimitInfo(headers)
		if info == nil {
			return
		}
		latest, err := h.lb.Store.GetAccount(ctx, accCopy.ID)
		if err != nil || latest == nil {
			slog.Warn("grok quota account reload failed", "account_id", accCopy.ID, "error", err)
			return
		}
		if !ApplyQuotaInfo(latest, info) {
			return
		}
		if err := h.lb.Store.UpdateAccount(ctx, latest); err != nil {
			slog.Warn("grok quota update failed", "account_id", latest.ID, "error", err)
		}
	}()
}
