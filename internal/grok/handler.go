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
	"time"
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

type Handler struct {
	base   *handler.BaseHandler
	cfg    *config.Config
	lb     *loadbalancer.LoadBalancer
	client *Client
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
		base:   handler.NewBaseHandler(lb),
		cfg:    cfg,
		lb:     lb,
		client: New(cfg),
	}
}

func (h *Handler) selectAccount(ctx context.Context) (*store.Account, string, error) {
	if h.lb == nil {
		return nil, "", fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannel(ctx, nil, "grok")
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
	if h != nil && h.lb != nil && h.lb.Store != nil {
		if m, err := h.lb.Store.GetModelByModelID(ctx, id); err == nil && m != nil {
			if !modelpolicy.IsVisibleGrokModel(id, m.Verified) {
				return fmt.Errorf("model not found")
			}
		} else if !modelpolicy.IsPublicGrokModelID(id) {
			return fmt.Errorf("model not found")
		}
	} else if !modelpolicy.IsPublicGrokModelID(id) {
		return fmt.Errorf("model not found")
	}
	return h.base.EnsureModelEnabled(ctx, id, "grok")
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
	if h == nil || h.lb == nil || h.lb.Store == nil || h.client == nil {
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
	if _, err := h.client.GetUsage(verifyCtx, sess.token, id); err != nil {
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
		slog.Info("Auto verified grok model", "model_id", id)
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
			return modelpolicy.IsVisibleGrokModel(id, m.Verified) && m.Status.Enabled()
		}
		slog.Warn("Auto verify grok model create failed", "model_id", id, "error", err)
		return false
	}

	slog.Info("Auto verified grok model", "model_id", id)
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
	return h.base.TrackAccount(acc)
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
	acc, err := h.lb.GetNextAccountExcludingByChannel(ctx, excludeIDs, "grok")
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
		resp, err := h.client.doChat(ctx, sess.token, payload)
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
		resp, err := h.client.doChat(ctx, sess.token, *payload)
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
	info := parseRateLimitInfo(headers)
	if info == nil {
		return
	}
	if !ApplyQuotaInfo(acc, info) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok quota update failed", "account_id", acc.ID, "error", err)
	}
}
