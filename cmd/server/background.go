package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/accountevents"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/refreshqueue"
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
	grokRefreshBackoffUntil time.Time
	// grokRefreshIntervalMin is the freshness window a verdict is expected to
	// hold for; the refresh loop sets it from the configured cadence.
	grokRefreshIntervalMin int
)

const (
	maxGrokRefreshPerCycle = 5
	grokRefresh429Backoff  = 10 * time.Minute
	grokRefreshPause       = 500 * time.Millisecond
	// grokRefreshDeadCredentialBackoff keeps a credential the upstream already
	// rejected out of the rotation. Re-asking once per tick burns a slot of the
	// per-cycle budget that a healthy account needs, and the answer cannot change
	// until the operator installs a new cookie (which resets LastAttempt).
	grokRefreshDeadCredentialBackoff = accountpolicy.CredentialReverify
)

// grokRefreshDeadCredential reports whether an account carries a credential the
// upstream definitively rejected and that has not been replaced since.
func grokRefreshDeadCredential(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.StatusCode) != "401" {
		return false
	}
	// A 401 without a verdict stamp has never been checked against a live
	// session, so it is not quarantined.
	if acc.VerifiedAt.IsZero() {
		return false
	}
	return !accountpolicy.NeedsReverify(acc, now)
}

type grokRefreshCandidate struct {
	token    string
	model    string
	accounts []*store.Account
}

// isUnverifiedGrokSSOAccount reports whether the row still has no health verdict:
// an enabled Web SSO account that has never been checked. "No verdict" is not a
// health state — scheduling it is what stops a freshly added account from looking
// fine while the account it was added next to shows the real failure.
func isUnverifiedGrokSSOAccount(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return false
	}
	if grok.ProviderForAccount(acc) != grok.ProviderWeb {
		return false
	}
	return accountpolicy.NeedsFirstVerdict(acc)
}

// firstCandidateAccount returns a representative account for a credential group,
// so a verdict can be classified with the right provider context.
func firstCandidateAccount(candidate grokRefreshCandidate) *store.Account {
	for _, acc := range candidate.accounts {
		if acc != nil {
			return acc
		}
	}
	return nil
}

// grokCandidateAccountIDs lists the account rows a credential group covers, so
// an operator log line names the affected accounts instead of only "grok failed".
func grokCandidateAccountIDs(accounts []*store.Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		ids = append(ids, acc.ID)
	}
	return ids
}

func buildGrokRefreshCandidates(accounts []*store.Account) []grokRefreshCandidate {
	now := time.Now()
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
		if grokRefreshDeadCredential(acc, now) {
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

// grokRefreshHub hands out one lease per account so the same credential is never
// refreshed twice at once. It is the process-wide hub, shared with the admin
// "check" path, so a manual check and the scheduler cannot race: a second, older
// snapshot winning the write-back is what could reinstate a status that had just
// been cleared.
var grokRefreshHub = refreshqueue.Default()

// planGrokRefreshCycle picks the refresh work for one cycle. It replaces the old
// global rotation offset: tasks are ordered by how long they have been due, the
// most overdue first, and an account already being refreshed is never scheduled
// again. Due time is derived from the verdict stamp — never verified counts as
// infinitely overdue, so a brand new account cannot sit behind the rotation.
func planGrokRefreshCycle(candidates []grokRefreshCandidate) []grokRefreshCandidate {
	if len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	tasks := make([]refreshqueue.Task, 0, len(candidates))
	for _, candidate := range candidates {
		acc := firstCandidateAccount(candidate)
		if acc == nil {
			continue
		}
		due := grokRefreshDue(acc, now)
		tasks = append(tasks, refreshqueue.Task{
			AccountID: acc.ID,
			Channel:   "grok",
			Due:       due,
			Stale:     acc.VerifiedAt.IsZero(),
			Payload:   candidate,
		})
	}
	planned := refreshqueue.Plan(tasks, grokRefreshHub, maxGrokRefreshPerCycle)
	out := make([]grokRefreshCandidate, 0, len(planned))
	for _, task := range planned {
		candidate, ok := task.Payload.(grokRefreshCandidate)
		if !ok {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

// grokRefreshDue reports how overdue an account's refresh is. A larger value is
// more urgent; an account that has never been verified is the most urgent of all.
func grokRefreshDue(acc *store.Account, now time.Time) time.Duration {
	if acc == nil {
		return 0
	}
	if acc.VerifiedAt.IsZero() {
		// "No verdict" is not a health state: treat it as maximally overdue.
		return 100 * 365 * 24 * time.Hour
	}
	interval := time.Duration(grokRefreshIntervalMinutes()) * time.Minute
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return now.Sub(acc.VerifiedAt.Add(interval))
}

// grokRefreshIntervalMinutes is the freshness window a verdict is expected to
// hold for. It mirrors the configured refresh cadence, so "due" means "older than
// one cycle" rather than an arbitrary constant.
func grokRefreshIntervalMinutes() int {
	grokRefreshMu.Lock()
	defer grokRefreshMu.Unlock()
	if grokRefreshIntervalMin <= 0 {
		return 30
	}
	return grokRefreshIntervalMin
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
				acc.StatusMessage = "上游已不接受该 OAuth 授权（refresh token 被拒绝），需要重新登录"
				acc.LastAttempt = time.Now()
				// Distinguish "the upstream invalidated this grant" from "our record
				// was wiped": both surface as 401, but only the former is expected
				// when a second login for the same xAI user rotates/retires the old
				// refresh token.
				slog.Warn("Grok CLI refresh token was rejected by the upstream; this account needs a new login",
					"account_id", acc.ID,
					"account_name", acc.Name,
					"email", acc.Email,
					"team_id", acc.TeamID,
					"has_refresh_token", strings.TrimSpace(acc.OAuthRefreshToken) != "",
					"token_fingerprint", grok.TokenFingerprint(acc.OAuthRefreshToken),
					"error", err)
			} else {
				slog.Warn("Auto refresh grok cli token failed", "account_id", acc.ID, "error", err)
			}
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

// grokSSORefreshRetryDelay is the pause before re-asking a rejected session in
// the background loop. A variable so tests can drive the retry without sleeping.
var grokSSORefreshRetryDelay = 800 * time.Millisecond

// retryGrokRefreshAttempt reads the SSO session identity, re-asking once when the
// upstream rejects the cookie, and reports whether the credential stands
// definitively rejected.
func retryGrokRefreshAttempt(ctx context.Context, client *grok.Client, token string) (grok.AccountIdentity, error) {
	attempt := func() (grok.AccountIdentity, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return client.FetchSessionIdentity(attemptCtx, token)
	}
	identity, err := attempt()
	if err == nil || !grok.IsAuthenticationFailure(err) {
		return identity, err
	}
	firstErr := err
	select {
	case <-ctx.Done():
		return grok.AccountIdentity{}, err
	case <-time.After(grokSSORefreshRetryDelay):
	}
	identity, err = attempt()
	if err == nil {
		slog.Warn("Auto refresh grok: session rejected once and accepted on retry; keeping the account",
			"token_fingerprint", grok.TokenFingerprint(token),
			"first_error", firstErr)
	}
	return identity, err
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

	batch := planGrokRefreshCycle(buildGrokRefreshCandidates(accounts))
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
		refreshGrokCandidate(ctx, cfg, s, grokClient, candidate)
	}
}

// refreshGrokCandidate refreshes one credential group while holding the account's
// lease, so a concurrent manual refresh cannot write an older snapshot over this
// one and reinstate a status that was just cleared.
func refreshGrokCandidate(ctx context.Context, cfg *config.Config, s *store.Store, grokClient *grok.Client, candidate grokRefreshCandidate) {
	leaseID := int64(0)
	if acc := firstCandidateAccount(candidate); acc != nil {
		leaseID = acc.ID
		if !grokRefreshHub.TryAcquire(leaseID) {
			slog.Debug("Auto refresh grok: account already refreshing; merged task", "account_id", leaseID)
			return
		}
		defer grokRefreshHub.Release(leaseID)
	}
	{
		// Session identity is the authentication check. Quota/model availability
		// is deliberately handled separately so a retired quota model cannot mark
		// a valid SSO account as HTTP 500. A single rejection is re-asked before it
		// is treated as final: the upstream also answers "unauthenticated" for
		// transient conditions, and a false verdict removes a working account from
		// the pool until an operator notices.
		identity, identityErr := retryGrokRefreshAttempt(ctx, grokClient, candidate.token)
		if identityErr != nil && grok.IsAuthenticationFailure(identityErr) {
			verdict := accountpolicy.Classify(firstCandidateAccount(candidate), identityErr, candidate.model)
			for _, acc := range candidate.accounts {
				if acc == nil {
					continue
				}
				verdict.Apply(acc)
				if err := s.UpdateAccount(ctx, acc); err != nil {
					slog.Warn("Auto refresh token: update account failed", "account_id", acc.ID, "type", "grok", "error", err)
				}
			}
			slog.Warn("Auto refresh grok SSO authentication failed",
				"status", verdict.Status,
				"needs_login", verdict.NeedsLogin,
				"account_ids", grokCandidateAccountIDs(candidate.accounts),
				"token_fingerprint", grok.TokenFingerprint(candidate.token),
				"error", identityErr)
			return
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
				verdict := accountpolicy.Classify(firstCandidateAccount(candidate), quotaErr, candidate.model)
				for _, acc := range candidate.accounts {
					if acc == nil {
						continue
					}
					verdict.Apply(acc)
					if err := s.UpdateAccount(ctx, acc); err != nil {
						slog.Warn("Auto refresh token: update account failed", "account_id", acc.ID, "type", "grok", "error", err)
					}
				}
				slog.Warn("Auto refresh grok SSO quota rejected the cookie",
					"status", verdict.Status,
					"needs_login", verdict.NeedsLogin,
					"account_ids", grokCandidateAccountIDs(candidate.accounts),
					"token_fingerprint", grok.TokenFingerprint(candidate.token),
					"error", quotaErr)
				return
			}
			// 404/model-unavailable and malformed quota responses are not auth
			// failures. Clear stale diagnostic 500/404 markers so a previous
			// model mismatch does not remain visible as an account failure.
			for _, acc := range candidate.accounts {
				if acc == nil {
					continue
				}
				if acc.StatusCode == "500" || acc.StatusCode == "404" {
					accountpolicy.Success(time.Now()).Apply(acc)
					if err := s.UpdateAccount(ctx, acc); err != nil {
						slog.Warn("Auto refresh token: clear stale grok status failed", "account_id", acc.ID, "error", err)
					}
				}
			}
			// Keep the account active and retain its last quota snapshot.
			slog.Warn("Auto refresh grok: Web quota unavailable; account remains active", "error", quotaErr)
			return
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
			// The credential answered: record the verdict so the account leaves the
			// first-verification queue and stops being treated as unknown. A quota
			// reset window still in the future keeps its reset stamp.
			keepQuotaReset := !acc.QuotaResetAt.IsZero() && time.Now().Before(acc.QuotaResetAt)
			quotaResetAt := acc.QuotaResetAt
			accountpolicy.Success(time.Now()).Apply(acc)
			if keepQuotaReset {
				acc.QuotaResetAt = quotaResetAt
			} else {
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
	grokRefreshMu.Lock()
	grokRefreshIntervalMin = int(interval.Minutes())
	grokRefreshMu.Unlock()
	slog.Debug("Auto refresh token enabled", "interval", interval.String())

	refreshAccounts := func() {
		accounts, err := s.GetEnabledAccounts(context.Background())
		if err != nil {
			slog.Error("Auto refresh token: list accounts failed", "error", err)
			return
		}
		grokRefreshQueue := make([]*store.Account, 0)
		// SSO accounts that carry no verdict at all. buildGrokRefreshCandidates
		// skips credentials the upstream already rejected, so these are collected
		// separately and always verified, newest first.
		grokPendingVerification := make([]*store.Account, 0)
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
			if isUnverifiedGrokSSOAccount(acc) {
				// Server-side completion for an account the UI submitted with the
				// async create header (and for any legacy row that never synced): the
				// admin page is not always open, so without this the row can sit with
				// no health verdict at all.
				grokPendingVerification = append(grokPendingVerification, acc)
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
		if len(grokPendingVerification) > 0 {
			// Newest first: the newest row is the account an operator just added and
			// is waiting on. These bypass the per-cycle rotation cap on purpose —
			// "no verdict yet" is not a health state, and leaving it unresolved is
			// what made a freshly added account look fine while an older one showed
			// the failure.
			ids := make([]int64, 0, len(grokPendingVerification))
			for _, acc := range grokPendingVerification {
				ids = append(ids, acc.ID)
			}
			slog.Info("Auto refresh grok: verifying accounts without a health verdict", "account_ids", ids)
			refreshGrokAccounts(context.Background(), cfg, s, grokPendingVerification)
		}
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
			case <-refreshKick.Channel():
				// An account was created, edited, deleted or rotated: the due set
				// changed, so waiting out the rest of the tick would be wrong. The
				// subscriber coalesces bursts, so a multi-field update wakes the
				// loop once.
				slog.Debug("Auto refresh token: account change detected; re-evaluating the due set")
				refreshAccounts()
			}
		}
	}()
}

// refreshKick wakes the refresh loop when an account changes, so a new account is
// picked up immediately instead of after up to one full interval.
var refreshKick = accountevents.NewKick()
