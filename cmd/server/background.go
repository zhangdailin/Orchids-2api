package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func preserveLatestAccountStatus(ctx context.Context, s *store.Store, acc *store.Account) {
	if s == nil || acc == nil || acc.ID == 0 {
		return
	}
	latest, err := s.GetAccount(ctx, acc.ID)
	if err != nil || latest == nil {
		return
	}

	latestStatus := strings.TrimSpace(latest.StatusCode)
	if latestStatus == "" {
		return
	}

	// Auto refresh works on a snapshot loaded at loop start. Preserve newer
	// request-path status markers so a successful token/quota sync does not
	// accidentally clear a recent blocked/cooldown state in Redis.
	if strings.TrimSpace(acc.StatusCode) == "" {
		acc.StatusCode = latestStatus
		acc.LastAttempt = latest.LastAttempt
	}
}

var (
	grokRefreshMu           sync.Mutex
	grokRefreshOffset       int
	grokRefreshBackoffUntil time.Time
)

const (
	maxGrokRefreshPerCycle = 5
	grokRefresh429Backoff  = 10 * time.Minute
	grokRefreshPause       = 500 * time.Millisecond
)

type grokRefreshCandidate struct {
	token    string
	model    string
	accounts []*store.Account
}

func buildGrokRefreshCandidates(accounts []*store.Account) []grokRefreshCandidate {
	byToken := make(map[string]int, len(accounts))
	candidates := make([]grokRefreshCandidate, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
			continue
		}
		// This loop can only collect Web SSO identity/quota. Linked Console
		// records require their own provider-specific health lifecycle and must
		// never inherit Web quota/status snapshots.
		if grok.ProviderForAccount(acc) != grok.ProviderWeb {
			continue
		}
		// Build CLI OAuth accounts refresh through their own token lifecycle
		// (refreshCLIAccounts) and must not be verified as SSO cookies here.
		if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
			continue
		}
		token := grok.NormalizeSSOToken(acc.ClientCookie)
		if token == "" {
			token = grok.NormalizeSSOToken(acc.RefreshToken)
		}
		if token == "" {
			continue
		}
		if idx, ok := byToken[token]; ok {
			candidates[idx].accounts = append(candidates[idx].accounts, acc)
			if candidates[idx].model == "" {
				candidates[idx].model = strings.TrimSpace(acc.AgentMode)
			}
			continue
		}
		byToken[token] = len(candidates)
		candidates = append(candidates, grokRefreshCandidate{
			token:    token,
			model:    strings.TrimSpace(acc.AgentMode),
			accounts: []*store.Account{acc},
		})
	}
	return candidates
}

func nextGrokRefreshBatch(candidates []grokRefreshCandidate, max int) []grokRefreshCandidate {
	if len(candidates) == 0 || max <= 0 {
		return nil
	}
	max = min(max, len(candidates))

	grokRefreshMu.Lock()
	start := grokRefreshOffset % len(candidates)
	grokRefreshOffset += max
	grokRefreshMu.Unlock()

	out := make([]grokRefreshCandidate, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, candidates[(start+i)%len(candidates)])
	}
	return out
}

func grokRefreshInBackoff(now time.Time) bool {
	grokRefreshMu.Lock()
	defer grokRefreshMu.Unlock()
	return !grokRefreshBackoffUntil.IsZero() && now.Before(grokRefreshBackoffUntil)
}

func setGrokRefreshBackoff(until time.Time) {
	grokRefreshMu.Lock()
	if until.After(grokRefreshBackoffUntil) {
		grokRefreshBackoffUntil = until
	}
	grokRefreshMu.Unlock()
}

// refreshCLIAccount refreshes a Build CLI OAuth account access token before it
// expires. The CLIClient oauth layer handles the refresh_token grant and
// persists rotated tokens back to Redis.
func refreshCLIAccount(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) {
	if acc == nil || s == nil || cfg == nil {
		return
	}
	// Persist the compatibility inference for pre-provider OAuth rows while we
	// already have a safe background update. This makes the product boundary
	// visible to the admin API instead of leaving old records ambiguous.
	grok.NormalizeProvider(acc)
	cliClient := grok.NewCLIClient(cfg)
	cliClient.SetAccountStore(s)
	if strings.TrimSpace(acc.OAuthAccessToken) == "" || acc.OAuthExpiresAt.IsZero() || time.Until(acc.OAuthExpiresAt) <= 5*time.Minute {
		refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		token, err := cliClient.OAuthAccessToken(refreshCtx, acc)
		cancel()
		if err != nil {
			if grok.IsCLIPermanentOAuthError(err) {
				acc.StatusCode = "401"
				acc.LastAttempt = time.Now()
			}
			slog.Warn("Auto refresh grok cli token failed", "account_id", acc.ID, "error", err)
			if updateErr := s.UpdateAccount(ctx, acc); updateErr != nil {
				slog.Warn("Auto refresh grok cli: update account failed", "account_id", acc.ID, "error", updateErr)
			}
			return
		}
		if token != "" {
			acc.OAuthAccessToken = token
			acc.StatusCode = ""
			acc.LastAttempt = time.Time{}
		}
	}

	billingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	billing, billingErr := cliClient.FetchBilling(billingCtx, acc)
	cancel()
	if billingErr != nil {
		slog.Warn("Auto sync grok cli billing failed; leaving numeric quota unavailable", "account_id", acc.ID, "error", billingErr)
	} else {
		grok.ApplyCLIBillingInfo(acc, billing)
	}
	if grok.CLIModelsNeedSync(acc, time.Now()) {
		modelsCtx, modelsCancel := context.WithTimeout(ctx, 15*time.Second)
		models, modelsErr := cliClient.FetchModels(modelsCtx, acc)
		modelsCancel()
		if modelsErr != nil {
			slog.Warn("Auto sync grok cli model catalog failed", "account_id", acc.ID, "error", modelsErr)
		} else {
			grok.ApplyCLIModels(acc, models, time.Now())
		}
	}
	if updateErr := s.UpdateAccount(ctx, acc); updateErr != nil {
		slog.Warn("Auto refresh grok cli: update account failed", "account_id", acc.ID, "error", updateErr)
	}
}

func refreshGrokAccounts(ctx context.Context, cfg *config.Config, s *store.Store, accounts []*store.Account) {
	if len(accounts) == 0 || s == nil {
		return
	}
	now := time.Now()
	if grokRefreshInBackoff(now) {
		slog.Debug("Auto refresh grok: skipped during rate-limit backoff")
		return
	}

	batch := nextGrokRefreshBatch(buildGrokRefreshCandidates(accounts), maxGrokRefreshPerCycle)
	if len(batch) == 0 {
		return
	}

	grokClient := grok.New(cfg)
	for _, candidate := range batch {
		select {
		case <-ctx.Done():
			return
		case <-time.After(grokRefreshPause):
		}

		// Session identity is the authentication check. Quota/model availability
		// is deliberately handled separately so a retired quota model cannot mark
		// a valid SSO account as HTTP 500.
		idCtx, idCancel := context.WithTimeout(ctx, 15*time.Second)
		identity, identityErr := grokClient.FetchSessionIdentity(idCtx, candidate.token)
		idCancel()
		if identityErr != nil && grok.IsAuthenticationFailure(identityErr) {
			statusCode := apperrors.ClassifyAccountStatus(identityErr.Error())
			if statusCode == "" {
				statusCode = "401"
			}
			for _, acc := range candidate.accounts {
				if acc == nil {
					continue
				}
				acc.StatusCode = statusCode
				acc.LastAttempt = time.Now()
				if err := s.UpdateAccount(ctx, acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account_id", acc.ID, "type", "grok", "error", err)
				}
			}
			slog.Warn("Auto refresh grok SSO authentication failed", "status", statusCode, "error", identityErr)
			continue
		}
		if identityErr != nil {
			slog.Debug("Auto refresh grok: session identity unavailable; continuing quota sync", "error", identityErr)
		}

		quotaCtx, quotaCancel := context.WithTimeout(ctx, 30*time.Second)
		windows, quotaErr := grokClient.GetWebQuota(quotaCtx, candidate.token)
		quotaCancel()
		if quotaErr != nil {
			statusCode := apperrors.ClassifyAccountStatus(quotaErr.Error())
			if statusCode == "429" {
				setGrokRefreshBackoff(time.Now().Add(grokRefresh429Backoff))
				slog.Warn("Auto refresh grok: Web quota rate-limited; pausing refresh", "backoff", grokRefresh429Backoff.String(), "error", quotaErr)
				return
			}
			if grok.IsAuthenticationFailure(quotaErr) {
				if statusCode == "" {
					statusCode = "401"
				}
				for _, acc := range candidate.accounts {
					if acc == nil {
						continue
					}
					acc.StatusCode = statusCode
					acc.LastAttempt = time.Now()
					if err := s.UpdateAccount(ctx, acc); err != nil {
						slog.Warn("Auto refresh token: update account failed", "account_id", acc.ID, "type", "grok", "error", err)
					}
				}
				continue
			}
			// 404/model-unavailable and malformed quota responses are not auth
			// failures. Clear stale diagnostic 500/404 markers so a previous
			// model mismatch does not remain visible as an account failure.
			for _, acc := range candidate.accounts {
				if acc == nil {
					continue
				}
				if acc.StatusCode == "500" || acc.StatusCode == "404" {
					acc.StatusCode = ""
					acc.LastAttempt = time.Time{}
					if err := s.UpdateAccount(ctx, acc); err != nil {
						slog.Warn("Auto refresh token: clear stale grok status failed", "account_id", acc.ID, "error", err)
					}
				}
			}
			// Keep the account active and retain its last quota snapshot.
			slog.Warn("Auto refresh grok: Web quota unavailable; account remains active", "error", quotaErr)
			continue
		}

		for _, acc := range candidate.accounts {
			if acc == nil {
				continue
			}
			if identityErr == nil {
				if identity.TeamID != "" {
					acc.TeamID = identity.TeamID
				}
				if identity.Email != "" {
					acc.Email = identity.Email
				}
				if identity.UserID != "" {
					acc.UserID = identity.UserID
				}
			}
			grok.ApplyWebQuotaInfo(acc, windows)
			if acc.QuotaResetAt.IsZero() || time.Now().After(acc.QuotaResetAt) {
				acc.StatusCode = ""
				acc.LastAttempt = time.Time{}
				acc.QuotaResetAt = time.Time{}
			}
			if err := s.UpdateAccount(ctx, acc); err != nil {
				slog.Warn("Auto refresh token: update account failed", "account_id", acc.ID, "type", "grok", "error", err)
			}
		}
	}
}

func startTokenRefreshLoop(ctx context.Context, cfg *config.Config, s *store.Store, lb *loadbalancer.LoadBalancer) {
	if !cfg.AutoRefreshToken {
		return
	}
	interval := time.Duration(cfg.TokenRefreshInterval) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	slog.Debug("Auto refresh token enabled", "interval", interval.String())

	refreshAccounts := func() {
		accounts, err := s.GetEnabledAccounts(context.Background())
		if err != nil {
			slog.Error("Auto refresh token: list accounts failed", "error", err)
			return
		}
		grokRefreshQueue := make([]*store.Account, 0)
		for _, acc := range accounts {
			if strings.EqualFold(acc.AccountType, "warp") {
				// nextRefreshTime from Warp's quota GraphQL response is a billing
				// period boundary, not a request backoff. Only skip an account when
				// the upstream actually returned 429 with Retry-After.
				if acc.StatusCode == "429" && !acc.QuotaResetAt.IsZero() && time.Now().Before(acc.QuotaResetAt) {
					continue
				}
				if strings.TrimSpace(acc.RefreshToken) == "" {
					continue
				}
				warpClient := warp.NewFromAccount(acc, cfg)
				_, err := warpClient.RefreshAccount(context.Background())
				if err != nil {
					retryAfter := warp.RetryAfter(err)
					httpStatus := warp.HTTPStatusCode(err)
					if httpStatus == 401 || httpStatus == 403 {
						lb.MarkAccountStatus(context.Background(), acc, fmt.Sprintf("%d", httpStatus))
					} else if retryAfter > 0 {
						acc.QuotaResetAt = time.Now().Add(retryAfter)
						if updateErr := s.UpdateAccount(context.Background(), acc); updateErr != nil {
							slog.Warn("Auto refresh token: record warp retry-after failed", "account", acc.Name, "type", "warp", "error", updateErr)
						}
					}
					slog.Warn("Auto refresh token failed", "account", acc.Name, "type", "warp", "http_status", httpStatus, "error", err)
					continue
				}
				warpClient.SyncAccountState()

				// Sync Warp usage quota via GraphQL
				limitCtx, limitCancel := context.WithTimeout(context.Background(), 15*time.Second)
				limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
				limitCancel()
				if limitErr != nil {
					slog.Warn("Warp usage sync failed", "account", acc.Name, "error", limitErr)
				} else if limitInfo != nil {
					warp.ApplyRequestLimitInfoToAccount(acc, limitInfo, bonuses)
					// If the GraphQL succeeded but returned no tier info and
					// the account has no subscription yet, default to free.
					if strings.TrimSpace(acc.Subscription) == "" || strings.EqualFold(acc.Subscription, "unknown") {
						acc.Subscription = "free"
					}
					slog.Debug("Warp usage synced", "account", acc.Name, "limit", acc.UsageLimit, "used", acc.UsageCurrent, "subscription", acc.Subscription)
				}

				preserveLatestAccountStatus(context.Background(), s, acc)

				if err := s.UpdateAccount(context.Background(), acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account", acc.Name, "type", "warp", "error", err)
				}
				continue
			}
			// Grok accounts: OAuth (Build CLI) refresh via their own token
			// lifecycle; SSO accounts check once per unique token.
			if strings.EqualFold(acc.AccountType, "grok") {
				if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
					refreshCLIAccount(context.Background(), cfg, s, acc)
				} else {
					grokRefreshQueue = append(grokRefreshQueue, acc)
				}
				continue
			}
			// Non-warp/non-grok account types are not auto-refreshed here.
			continue
		}
		refreshGrokAccounts(context.Background(), cfg, s, grokRefreshQueue)
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in token refresh loop", "error", err)
			}
		}()
		refreshAccounts()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshAccounts()
			}
		}
	}()
}
