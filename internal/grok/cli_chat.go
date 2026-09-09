package grok

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/store"
)

func (h *Handler) cliBaseURL() string {
	if h != nil && h.cfg != nil {
		return h.cfg.GrokCLIBaseURLOrDefault()
	}
	return defaultCLIBaseURL
}

func (h *Handler) cliHeaders(acc *store.Account, token string) http.Header {
	if h == nil || h.cliClient == nil {
		return nil
	}
	return h.cliClient.cliHeaders(acc, token)
}

// doCLIWithAutoSwitchAt issues a CLI request, switching to another OAuth account
// on transient failures (401 after refresh, 5xx) while treating team-level 429
// as shared (no switch).
func (h *Handler) doCLIWithAutoSwitchAt(ctx context.Context, sess *chatAccountSession, payload map[string]interface{}, modelID, path string) (*http.Response, error) {
	ctx = withReasoningDiagnostics(ctx, payload)
	if sess == nil || sess.acc == nil {
		return nil, fmt.Errorf("empty cli chat session")
	}
	if h == nil || h.cliClient == nil {
		return nil, fmt.Errorf("grok cli client not configured")
	}
	return h.retryWithAccountSwitch(ctx, sess, 1500*time.Millisecond,
		func() (*http.Response, error) { return h.cliClient.doResponsesAt(ctx, sess.acc, path, payload) },
		func(used []int64) (*chatAccountSession, error) { return h.openCLIAccountSession(ctx, used, modelID) }, nil)
}

func (h *Handler) openCLIAccountSessionByID(ctx context.Context, accountID int64, modelID string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil || accountID == 0 {
		return nil, fmt.Errorf("stored response account is unavailable")
	}
	acc, err := h.lb.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("stored response account is unavailable: %w", err)
	}
	if acc == nil || !acc.Enabled || ProviderForAccount(acc) != ProviderBuild || !AccountSupportsModel(acc, modelID) || !h.routeAllowsAccount(ctx, modelID, acc.ID) || !h.accountCapacityAvailable(acc) {
		return nil, fmt.Errorf("stored response account is unavailable")
	}
	token := strings.TrimSpace(acc.OAuthAccessToken)
	if token == "" {
		token = strings.TrimSpace(acc.OAuthRefreshToken)
	}
	if token == "" {
		return nil, fmt.Errorf("stored response account token is empty")
	}
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return nil, fmt.Errorf("stored response account is at its concurrency limit")
	}
	return &chatAccountSession{acc: acc, token: token, release: release}, nil
}

// openCLIAccountSession selects the next available Build CLI OAuth account.
func (h *Handler) openCLIAccountSession(ctx context.Context, excludeIDs []int64, modelID string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	if pinnedID := h.affinityAccount(ctx, ProviderBuild); pinnedID != 0 && !containsAccountID(excludeIDs, pinnedID) {
		if pinned, err := h.openCLIAccountSessionByID(ctx, pinnedID, modelID); err == nil {
			if accountAffinityUsable(pinned.acc) {
				return pinned, nil
			}
			pinned.Close()
		}
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTracker, func(acc *store.Account) bool {
		return acc != nil && ProviderForAccount(acc) == ProviderBuild && AccountSupportsModel(acc, modelID) && h.routeAllowsAccount(ctx, modelID, acc.ID)
	})
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(acc.OAuthAccessToken)
	if token == "" {
		token = strings.TrimSpace(acc.OAuthRefreshToken)
	}
	if token == "" {
		return nil, fmt.Errorf("grok cli account token is empty")
	}
	h.bindAffinity(ctx, ProviderBuild, acc.ID)
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return h.openCLIAccountSession(ctx, append(excludeIDs, acc.ID), modelID)
	}
	return &chatAccountSession{
		acc:            acc,
		token:          token,
		poolCandidates: nil,
		release:        release,
	}, nil
}

// openConsoleAccountSession selects a Console SSO account. Console sessions
// cannot be substituted with Web SSO cookies even when both belong to the
// same person, because their endpoints and quotas are independent.
func (h *Handler) openConsoleAccountSession(ctx context.Context, excludeIDs []int64, modelIDs ...string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	modelID := ""
	if len(modelIDs) > 0 {
		modelID = strings.TrimSpace(modelIDs[0])
	}
	allowed := func(acc *store.Account) bool {
		return isGrokConsoleAccount(acc) && (modelID == "" || h.routeAllowsAccount(ctx, modelID, acc.ID))
	}
	if pinnedID := h.affinityAccount(ctx, ProviderConsole); pinnedID != 0 && !containsAccountID(excludeIDs, pinnedID) && h.lb.Store != nil {
		if pinned, err := h.lb.Store.GetAccount(ctx, pinnedID); err == nil && pinned != nil && pinned.Enabled && allowed(pinned) && accountAffinityUsable(pinned) && h.accountCapacityAvailable(pinned) {
			token := grokSSOTokenRaw(pinned)
			if NormalizeSSOToken(token) != "" {
				if release, reserved := h.reserveAccount(pinned); reserved {
					return &chatAccountSession{acc: pinned, token: token, release: release}, nil
				}
			}
		}
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTracker, allowed)
	if err != nil {
		return nil, err
	}
	token := grokSSOTokenRaw(acc)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("grok console account token is empty")
	}
	h.bindAffinity(ctx, ProviderConsole, acc.ID)
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return h.openConsoleAccountSession(ctx, append(excludeIDs, acc.ID), modelIDs...)
	}
	return &chatAccountSession{acc: acc, token: token, release: release}, nil
}

func containsAccountID(values []int64, id int64) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

func accountAffinityUsable(acc *store.Account) bool {
	if acc == nil || !acc.Enabled {
		return false
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return true
	}
	return !acc.QuotaResetAt.IsZero() && time.Now().After(acc.QuotaResetAt)
}
