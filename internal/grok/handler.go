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
	acc     *store.Account
	token   string
	release func()
}

type imageEditUploadInput struct {
	mime string
	data []byte
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
	token := NormalizeSSOToken(raw)
	if token == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, token, nil
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
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

	m, err := h.lb.Store.GetModelByModelID(ctx, id)
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

func isAutoRegisterableGrokModel(modelID string) bool {
	id := normalizeModelID(modelID)
	if id == "" {
		return false
	}
	if strings.HasPrefix(id, "grok-imagine-") {
		return false
	}
	return strings.HasPrefix(id, "grok-")
}

func (h *Handler) tryAutoRegisterModel(ctx context.Context, modelID string) bool {
	client := h.currentClient()
	if h == nil || h.lb == nil || h.lb.Store == nil || client == nil {
		return false
	}
	id := normalizeModelID(modelID)
	if !isAutoRegisterableGrokModel(id) {
		return false
	}
	if _, ok := ResolveModelOrDynamic(id); !ok {
		return false
	}

	if existing, err := h.lb.Store.GetModelByModelID(ctx, id); err == nil && existing != nil {
		if modelpolicy.IsVisibleGrokModel(id, existing.Verified) && existing.Status.Enabled() {
			h.cacheValidatedModel(id)
			return true
		}
	}

	sess, err := h.openChatAccountSession(ctx)
	if err != nil {
		return false
	}
	defer sess.Close()

	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := client.VerifyToken(verifyCtx, sess.token, id); err != nil {
		slog.Debug("Auto verify grok model failed", "model_id", id, "error", err)
		return false
	}

	name := id
	if spec, ok := ResolveModelOrDynamic(id); ok && strings.TrimSpace(spec.Name) != "" {
		name = strings.TrimSpace(spec.Name)
	}

	if existing, err := h.lb.Store.GetModelByModelID(ctx, id); err == nil && existing != nil {
		existing.Channel = "Grok"
		existing.Name = name
		existing.Status = store.ModelStatusAvailable
		existing.Verified = true
		if err := h.lb.Store.UpdateModel(ctx, existing); err != nil {
			slog.Warn("Auto verify grok model update failed", "model_id", id, "error", err)
			return false
		}
		h.cacheValidatedModel(id)
		slog.Debug("Auto verified grok model", "model_id", id)
		return true
	}

	newModel := &store.Model{
		Channel:  "Grok",
		ModelID:  id,
		Name:     name,
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}
	if err := h.lb.Store.CreateModel(ctx, newModel); err != nil {
		if m, checkErr := h.lb.Store.GetModelByModelID(ctx, id); checkErr == nil && m != nil {
			if modelpolicy.IsVisibleGrokModel(id, m.Verified) && m.Status.Enabled() {
				h.cacheValidatedModel(id)
			}
			return modelpolicy.IsVisibleGrokModel(id, m.Verified) && m.Status.Enabled()
		}
		slog.Warn("Auto verify grok model create failed", "model_id", id, "error", err)
		return false
	}

	h.cacheValidatedModel(id)
	slog.Debug("Auto verified grok model", "model_id", id)
	return true
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

func (h *Handler) openChatAccountSession(ctx context.Context) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcluding(ctx, nil)
}

func (h *Handler) openChatAccountSessionExcluding(ctx context.Context, excludeIDs []int64) (*chatAccountSession, error) {
	if h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTracker(ctx, excludeIDs, "grok", h.connTracker)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	token := NormalizeSSOToken(raw)
	if token == "" {
		return nil, fmt.Errorf("grok account token is empty")
	}
	return &chatAccountSession{
		acc:     acc,
		token:   token,
		release: h.trackAccount(acc),
	}, nil
}

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// doChatWithAutoSwitch runs one chat request and switches account once on 403/429.
func (h *Handler) doChatWithAutoSwitch(ctx context.Context, sess *chatAccountSession, payload map[string]interface{}) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
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
		resp, err := client.doChat(ctx, sess.token, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		h.markAccountStatus(ctx, sess.acc, err)
		if !shouldSwitchGrokAccount(err) || attempt == maxAttempts-1 {
			return nil, err
		}
		sess.Close()
		next, err2 := h.openChatAccountSessionExcluding(ctx, used)
		if err2 != nil {
			return nil, fmt.Errorf("account switch failed: %w (original: %v)", err2, err)
		}
		sess.acc = next.acc
		sess.token = next.token
		sess.release = next.release
	}
	return nil, lastErr
}

// doChatWithAutoSwitchRebuild retries once with a switched account and rebuilds payload for the new token.
func (h *Handler) doChatWithAutoSwitchRebuild(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]interface{},
	rebuild func(token string) (map[string]interface{}, error),
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
		resp, err := client.doChat(ctx, sess.token, *payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		h.markAccountStatus(ctx, sess.acc, err)
		if !shouldSwitchGrokAccount(err) || attempt == maxAttempts-1 {
			return nil, err
		}

		sess.Close()
		next, err2 := h.openChatAccountSessionExcluding(ctx, used)
		if err2 != nil {
			return nil, err
		}
		sess.acc = next.acc
		sess.token = next.token
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

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	status := classifyAccountStatusFromError(err.Error())
	if status == "403" || status == "429" {
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
		if !ApplyQuotaInfo(&accCopy, info) {
			return
		}
		if err := h.lb.Store.UpdateAccount(ctx, &accCopy); err != nil {
			slog.Warn("grok quota update failed", "account_id", accCopy.ID, "error", err)
		}
	}()
}
