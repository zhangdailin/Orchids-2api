package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/audit"
	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok"
	"orchids-api/internal/middleware"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
	"orchids-api/internal/tokencache"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

type API struct {
	store        *store.Store
	tokenCache   tokencache.Cache
	promptCache  tokencache.PromptCache
	adminUser    string
	adminPass    string
	loginLimiter *middleware.RateLimiter
	config       atomic.Pointer[config.Config]

	// Account check backoff / storm control
	checkMu          sync.Mutex
	checkInFlight    map[int64]bool
	checkFailCount   map[int64]int
	checkNextAllowed map[int64]time.Time
	checkSem         chan struct{}

	// Warp device logins hold only short-lived, in-memory device codes. A
	// completed login persists the resulting refresh_token as a normal account.
	warpDeviceLoginMu sync.Mutex
	warpDeviceLogins  map[string]*warpDeviceLogin

	// Grok device logins are separate from Warp so their OAuth device codes and
	// credentials can never cross authentication flows.
	grokDeviceLoginMu sync.Mutex
	grokDeviceLogins  map[string]*grokDeviceLogin

	// WorkBuddy logins hold an OAuth state transaction plus the resulting
	// credentials until the account is verified and persisted.
	workbuddyLoginMu sync.Mutex
	workbuddyLogins  map[string]*workbuddyLogin
}

type auditEventRecord struct {
	ID    string      `json:"id"`
	Event audit.Event `json:"event"`
}

// auditScanCap bounds how many stream entries one query reads before filtering.
// The ledger is time-bounded, so a filtered page must not silently promise the
// whole history: coverage tells the reader what the store actually holds.
const auditScanCap = 2000

// HandleAuditEvents exposes the bounded Redis audit ledger to authenticated
// administrators. Cursor pagination uses Redis Stream IDs; the journal is
// filtered by kind (request/operation/system) plus the fields the log centre
// offers. Credentials never appear: whether they do is enforced at write time by
// audit.SummarizeChange.
func (a *API) HandleAuditEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		http.Error(w, "audit ledger requires Redis storage", http.StatusServiceUnavailable)
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	maxID := "+"
	if before := strings.TrimSpace(r.URL.Query().Get("before")); before != "" {
		maxID = "(" + before
	}
	filter := auditFilterFromQuery(r)

	scanCount := int64(limit) * 5
	if scanCount > auditScanCap {
		scanCount = auditScanCap
	}
	messages, err := a.store.RedisClient().XRevRangeN(r.Context(), a.store.RedisPrefix()+"audit:log", maxID, "-", scanCount).Result()
	if err != nil {
		http.Error(w, "failed to read audit ledger", http.StatusInternalServerError)
		return
	}
	records := make([]auditEventRecord, 0, len(messages))
	scanned := 0
	for _, message := range messages {
		scanned++
		raw, _ := message.Values["data"].(string)
		var event audit.Event
		if raw == "" || json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		if !filter.matches(event) {
			continue
		}
		records = append(records, auditEventRecord{ID: message.ID, Event: event})
		if len(records) >= limit {
			break
		}
	}
	nextCursor := ""
	if len(records) == limit && len(messages) > 0 {
		nextCursor = records[len(records)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":        records,
		"next_cursor": nextCursor,
		"scanned":     scanned,
		// Filtered: true means the store was scanned to its cap, so an empty page
		// is "nothing matched inside the retained window" — not "never happened".
		"filtered":   scanned >= int(scanCount),
		"scan_cap":   scanCount,
		"coverage":   a.auditCoverage(r.Context()),
		"filter_used": filter.describe(),
	})
}

// auditQueryFilter is the log centre's filter set.
type auditQueryFilter struct {
	kind      string
	channel   string
	status    string
	action    string
	actor     string
	model     string
	accountID int64
	apiKeyID  int64
}

func auditFilterFromQuery(r *http.Request) auditQueryFilter {
	query := r.URL.Query()
	parse := func(name string) int64 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(query.Get(name)), 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	}
	return auditQueryFilter{
		kind:      strings.ToLower(strings.TrimSpace(query.Get("kind"))),
		channel:   strings.ToLower(strings.TrimSpace(query.Get("channel"))),
		status:    strings.ToLower(strings.TrimSpace(query.Get("status"))),
		action:    strings.ToLower(strings.TrimSpace(query.Get("action"))),
		actor:     strings.ToLower(strings.TrimSpace(query.Get("actor"))),
		model:     strings.ToLower(strings.TrimSpace(query.Get("model"))),
		accountID: parse("account_id"),
		apiKeyID:  parse("api_key_id"),
	}
}

func (f auditQueryFilter) matches(event audit.Event) bool {
	if f.kind != "" && string(event.Kind) != f.kind {
		return false
	}
	if f.channel != "" && !strings.EqualFold(event.Channel, f.channel) {
		return false
	}
	if f.status != "" && !strings.EqualFold(event.Status, f.status) {
		return false
	}
	if f.action != "" && !strings.Contains(strings.ToLower(event.Action), f.action) {
		return false
	}
	if f.actor != "" && !strings.Contains(strings.ToLower(event.Actor), f.actor) {
		return false
	}
	if f.model != "" && !strings.Contains(strings.ToLower(event.Model), f.model) {
		return false
	}
	if f.accountID != 0 && event.AccountID != f.accountID {
		return false
	}
	if f.apiKeyID != 0 && event.APIKeyID != f.apiKeyID {
		return false
	}
	return true
}

func (f auditQueryFilter) describe() map[string]interface{} {
	described := map[string]interface{}{}
	if f.kind != "" {
		described["kind"] = f.kind
	}
	if f.channel != "" {
		described["channel"] = f.channel
	}
	if f.status != "" {
		described["status"] = f.status
	}
	if f.action != "" {
		described["action"] = f.action
	}
	if f.actor != "" {
		described["actor"] = f.actor
	}
	if f.model != "" {
		described["model"] = f.model
	}
	if f.accountID != 0 {
		described["account_id"] = f.accountID
	}
	if f.apiKeyID != 0 {
		described["api_key_id"] = f.apiKeyID
	}
	return described
}

// auditCoverage reports the retained window and the per-journal counts, so the
// UI can state what the numbers actually cover instead of promising a fixed
// retention period.
func (a *API) auditCoverage(ctx context.Context) map[string]interface{} {
	coverage := map[string]interface{}{"entries": 0, "oldest": nil, "newest": nil, "counts": map[string]int{}}
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		return coverage
	}
	client := a.store.RedisClient()
	key := a.store.RedisPrefix() + "audit:log"

	total, err := client.XLen(ctx, key).Result()
	if err != nil {
		return coverage
	}
	coverage["entries"] = total
	counts := map[string]int{}
	var newest, oldest string
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", 1).Result(); err == nil && len(entries) > 0 {
		newest = entries[0].ID
	}
	if entries, err := client.XRangeN(ctx, key, "-", "+", 1).Result(); err == nil && len(entries) > 0 {
		oldest = entries[0].ID
	}
	coverage["oldest"] = streamIDTime(oldest)
	coverage["newest"] = streamIDTime(newest)

	// Per-journal counts over the retained window. The cap keeps the call bounded
	// on a busy instance; counts are therefore a floor, which the response says.
	if entries, err := client.XRevRangeN(ctx, key, "+", "-", auditScanCap).Result(); err == nil {
		coverage["count_sampled"] = len(entries)
		for _, entry := range entries {
			raw, _ := entry.Values["data"].(string)
			var event audit.Event
			if raw == "" || json.Unmarshal([]byte(raw), &event) != nil {
				continue
			}
			kind := string(event.Kind)
			if kind == "" {
				kind = string(audit.KindRequest)
			}
			counts[kind]++
		}
	}
	coverage["counts"] = counts
	return coverage
}

// streamIDTime converts a Redis Stream ID ("ms-seq") into RFC3339, or null.
func streamIDTime(id string) interface{} {
	millis := strings.SplitN(strings.TrimSpace(id), "-", 2)[0]
	if millis == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(millis, 10, 64)
	if err != nil || parsed <= 0 {
		return nil
	}
	return time.UnixMilli(parsed).UTC().Format(time.RFC3339)
}

const maxDeviceLogins = 10

type deviceLogin struct {
	deviceCode string
	userCode   string
	verifyURI  string
	verifyFull string
	expiresAt  time.Time
	interval   time.Duration
	cancel     context.CancelFunc

	status    string
	message   string
	accountID int64

	// enabled/enabledKnown carry a provider-specific preference captured at
	// start time (currently the WorkBuddy account enabled flag).
	enabled      bool
	enabledKnown bool
}

type deviceLoginResponse struct {
	ID                      string `json:"id"`
	Status                  string `json:"status"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	AccountID               int64  `json:"account_id,omitempty"`
	Message                 string `json:"message,omitempty"`
}

type warpDeviceLogin = deviceLogin
type grokDeviceLogin = deviceLogin
type workbuddyLogin = deviceLogin

var puterFetchMonthlyUsage = func(ctx context.Context, acc *store.Account, cfg *config.Config) (*puter.MonthlyUsage, error) {
	client := puter.NewFromAccount(acc, cfg)
	defer client.Close()
	return client.FetchMonthlyUsage(ctx)
}

func verifyGrokAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore *store.Store) error {
	if acc == nil {
		return fmt.Errorf("missing grok account")
	}
	// Build CLI OAuth accounts verify against the CLI proxy with a Bearer token.
	if grokAccountIsOAuth(acc) {
		if !grokAccountHasOAuthCredentials(acc) {
			return fmt.Errorf("missing oauth token")
		}
		cliClient := grok.NewCLIClient(cfg)
		cliClient.SetAccountStore(accountStore)
		verifyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		status, verifyErr := cliClient.VerifyAccount(verifyCtx, acc)
		cancel()
		if verifyErr != nil {
			if status != "" {
				return fmt.Errorf("%s: %w", status, verifyErr)
			}
			return verifyErr
		}
		// Billing is a separate, optional official CLI endpoint. Failure to read
		// its percentage window must not turn an authenticated account into a
		// false 401 or make up a subscription allowance.
		if billing, billingErr := cliClient.FetchBilling(ctx, acc); billingErr != nil {
			slog.Warn("Grok CLI billing sync failed; leaving quota unavailable", "account_id", acc.ID, "error", billingErr)
		} else {
			grok.ApplyCLIBillingInfo(acc, billing)
		}
		modelsCtx, modelsCancel := context.WithTimeout(ctx, 15*time.Second)
		if models, modelsErr := cliClient.FetchModels(modelsCtx, acc); modelsErr != nil {
			slog.Warn("Grok CLI model catalog sync failed", "account_id", acc.ID, "error", modelsErr)
		} else {
			grok.ApplyCLIModels(acc, models, time.Now())
		}
		modelsCancel()
		return nil
	}

	credential := strings.TrimSpace(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
	if grok.NormalizeSSOToken(credential) == "" {
		return fmt.Errorf("missing sso token")
	}
	acc.ClientCookie = credential

	client := grok.New(cfg)
	// Session identity is the authentication check. Quota availability is a
	// separate concern and must not be allowed to invalidate a valid cookie.
	identity, identityErr, authRejected := fetchGrokSSOIdentity(ctx, client, credential)
	if authRejected {
		return fmt.Errorf("%s: %w", classifyGrokAuthStatus(identityErr), identityErr)
	}
	if identityErr == nil {
		if identity.UserID != "" {
			acc.UserID = identity.UserID
		}
		if identity.Email != "" {
			acc.Email = identity.Email
		}
		if identity.TeamID != "" {
			acc.TeamID = identity.TeamID
		}
	} else {
		slog.Warn("Grok SSO identity sync unavailable; continuing with quota sync", "account_id", acc.ID, "error", identityErr)
	}

	quotaCtx, quotaCancel := context.WithTimeout(ctx, 25*time.Second)
	windows, quotaErr := client.GetWebQuota(quotaCtx, credential)
	quotaCancel()
	if quotaErr != nil {
		if grok.IsAuthenticationFailure(quotaErr) {
			// The quota endpoint is the second witness: require it to reject the
			// cookie twice as well before the account is declared unauthorized.
			if second, secondErr := grokWebQuotaWithRetry(client, credential, ctx); secondErr == nil {
				grok.ApplyWebQuotaInfo(acc, second)
				return nil
			} else if grok.IsAuthenticationFailure(secondErr) {
				quotaErr = secondErr
			}
			return fmt.Errorf("%s: %w", classifyGrokAuthStatus(quotaErr), quotaErr)
		}
		// A quota read can be rate limited or unsupported; neither means the
		// credential is invalid. Keep the account usable and let the caller
		// classify whatever error carries.
		slog.Warn("Grok SSO quota unavailable; account remains authenticated", "account_id", acc.ID, "error", quotaErr)
		return nil
	}
	grok.ApplyWebQuotaInfo(acc, windows)
	return nil
}

// grokSSOAuthRetryDelay is the pause before re-asking a rejected session, giving
// an upstream hiccup a chance to clear before an account is declared dead.
// A variable so tests can drive the retry without sleeping.
var grokSSOAuthRetryDelay = 800 * time.Millisecond

// retryGrokSSOAuthAttempt runs one upstream attempt and, when it reports an
// authentication rejection, runs it once more. A single rejection is not proof:
// the upstream answers "unauthenticated" for transient conditions too, and
// treating one bad answer as final takes a working account out of the pool until
// an operator notices. Returns whether the credential stands definitively
// rejected after the retry.
func retryGrokSSOAuthAttempt(attempt func() (grok.AccountIdentity, error)) (grok.AccountIdentity, error, bool) {
	identity, err := attempt()
	if err == nil || !grok.IsAuthenticationFailure(err) {
		return identity, err, false
	}
	firstErr := err
	time.Sleep(grokSSOAuthRetryDelay)
	identity, err = attempt()
	switch {
	case err == nil:
		slog.Warn("Grok SSO session rejected once and accepted on retry; keeping the account",
			"first_error", firstErr)
		return identity, nil, false
	case grok.IsAuthenticationFailure(err):
		return grok.AccountIdentity{}, err, true
	default:
		return grok.AccountIdentity{}, err, false
	}
}

// fetchGrokSSOIdentity resolves the session identity with the retry policy above.
func fetchGrokSSOIdentity(ctx context.Context, client *grok.Client, credential string) (grok.AccountIdentity, error, bool) {
	return retryGrokSSOAuthAttempt(func() (grok.AccountIdentity, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return client.FetchSessionIdentity(attemptCtx, credential)
	})
}

// grokWebQuotaWithRetry reads the Web quota, retrying once on an authentication
// rejection so the second verdict matches the identity check's strictness.
func grokWebQuotaWithRetry(client *grok.Client, credential string, ctx context.Context) (map[string]*grok.RateLimitInfo, error) {
	read := func() (map[string]*grok.RateLimitInfo, error) {
		quotaCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		return client.GetWebQuota(quotaCtx, credential)
	}
	if windows, err := read(); err == nil {
		return windows, nil
	} else if !grok.IsAuthenticationFailure(err) {
		return nil, err
	}
	time.Sleep(grokSSOAuthRetryDelay)
	windows, err := read()
	if err == nil {
		slog.Warn("Grok SSO quota rejected once and accepted on retry; keeping the account")
	}
	return windows, err
}

// classifyGrokAuthStatus maps a definitive SSO authentication failure to "401".
func classifyGrokAuthStatus(err error) string {
	if err == nil {
		return ""
	}
	if status := apperrors.ClassifyAccountStatus(err.Error()); status != "" {
		return status
	}
	return "401"
}

func normalizeWarpTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "warp") {
		return
	}
	acc.RefreshToken = warp.RefreshToken(acc)
	// Only the official device-login flow supplies Warp session credentials.
	// Clear legacy fields so they cannot become alternate authentication sources.
	acc.Token = ""
	acc.ClientCookie = ""
	acc.SessionCookie = ""
}

func normalizeWarpTokenOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	copyAcc := *acc
	if strings.EqualFold(strings.TrimSpace(copyAcc.AccountType), "warp") {
		// Browser-login session credentials are private, including on export.
		copyAcc.RefreshToken = ""
		copyAcc.Token = ""
		copyAcc.ClientCookie = ""
		copyAcc.SessionCookie = ""
		copyAcc.OAuthAccessToken = ""
		copyAcc.OAuthRefreshToken = ""
	}
	return &copyAcc
}

func httpStatusFromAccountStatus(status string) int {
	switch strings.TrimSpace(status) {
	case "401":
		return http.StatusUnauthorized
	case "402":
		return http.StatusPaymentRequired
	case "403":
		return http.StatusForbidden
	case "404":
		return http.StatusNotFound
	case "429":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func normalizeGrokTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	// OAuth (Build CLI) accounts carry access/refresh tokens and must not be
	// treated as SSO cookies.
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		acc.GrokProvider = grok.ProviderBuild
		acc.OAuthAccessToken = strings.TrimSpace(acc.OAuthAccessToken)
		acc.OAuthRefreshToken = strings.TrimSpace(acc.OAuthRefreshToken)
		acc.ClientCookie = ""
		acc.RefreshToken = ""
		acc.SessionCookie = ""
		acc.SessionID = ""
		acc.ClientUat = ""
		acc.ProjectID = ""
		return
	}
	// Any non-OAuth Grok credential is the Web SSO mode. Persist the explicit
	// type so legacy imports do not appear as an unclassified account.
	acc.CredentialType = "sso"
	switch strings.ToLower(strings.TrimSpace(acc.GrokProvider)) {
	case grok.ProviderWeb, grok.ProviderConsole:
		acc.GrokProvider = strings.ToLower(strings.TrimSpace(acc.GrokProvider))
	default:
		// Do not retain arbitrary provider labels: routing must have a single
		// explicit product boundary for every SSO credential.
		acc.GrokProvider = grok.ProviderWeb
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	if grok.NormalizeSSOToken(raw) == "" {
		acc.ClientCookie = ""
	} else {
		acc.ClientCookie = raw
	}
	// Grok app-chat can benefit from the full browser cookie stored in ClientCookie.
	acc.RefreshToken = ""
	acc.SessionCookie = ""
	acc.SessionID = ""
	acc.ClientUat = ""
	acc.ProjectID = ""
}

// grokAccountIsOAuth reports whether a Grok account is a Build CLI OAuth account.
func grokAccountIsOAuth(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth")
}

// grokAccountHasOAuthCredentials reports whether an OAuth account carries at
// least one usable token after normalization.
func grokAccountHasOAuthCredentials(acc *store.Account) bool {
	if !grokAccountIsOAuth(acc) {
		return false
	}
	return strings.TrimSpace(acc.OAuthAccessToken) != "" || strings.TrimSpace(acc.OAuthRefreshToken) != ""
}

// preserveGrokOAuthCredentials keeps existing OAuth secrets when the admin UI
// submits empty fields (secrets are redacted on read and therefore absent on
// ordinary edit/save).
func preserveGrokOAuthCredentials(acc, existing *store.Account) {
	if acc == nil || existing == nil || !grokAccountIsOAuth(acc) {
		return
	}
	if strings.TrimSpace(acc.OAuthAccessToken) == "" {
		acc.OAuthAccessToken = existing.OAuthAccessToken
	}
	if strings.TrimSpace(acc.OAuthRefreshToken) == "" {
		acc.OAuthRefreshToken = existing.OAuthRefreshToken
	}
	if acc.OAuthExpiresAt.IsZero() && !existing.OAuthExpiresAt.IsZero() {
		acc.OAuthExpiresAt = existing.OAuthExpiresAt
	}
	if strings.TrimSpace(acc.TeamID) == "" {
		acc.TeamID = existing.TeamID
	}
	if strings.TrimSpace(acc.UpstreamMode) == "" {
		acc.UpstreamMode = existing.UpstreamMode
	}
}

// grokSSOCookieValue normalizes the SSO cookie an account carries, so a
// credential comparison ignores decoration and field placement.
func grokSSOCookieValue(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	return grok.NormalizeSSOToken(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
}

// preserveGrokRuntimeStateOnAdminEdit keeps provider-observed state out of the
// generic account edit surface. The management modal only changes credential
// and operator configuration; a partial PUT must not erase a linked Console
// account's independent catalog, quota, health, or recovery state.
//
// One state pair is credential-scoped rather than runtime-scoped: the recorded
// status and its reason describe the credential they were observed with. A PUT
// that installs a DIFFERENT SSO cookie supersedes them, so they are reset and the
// next sync re-verifies. Keeping them made a repaired account display the old
// 未授权 badge until a manual Sync or the auto-sync TTL expired.
func preserveGrokRuntimeStateOnAdminEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	credentialReplaced := grokSSOCookieValue(acc) != grokSSOCookieValue(existing)
	acc.Token = existing.Token
	acc.Subscription = existing.Subscription
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	acc.UsageLimit = existing.UsageLimit
	if !credentialReplaced {
		acc.StatusCode = existing.StatusCode
		// The reason is server-observed, not operator input: an edit form that
		// carries no status fields must not blank the explanation of the status
		// it keeps.
		acc.StatusMessage = existing.StatusMessage
		acc.LastAttempt = existing.LastAttempt
		acc.VerifiedAt = existing.VerifiedAt
	} else {
		// A new credential has no verdict yet; ask the store to drop the stored one
		// so the scheduler verifies it instead of trusting the old result.
		acc.ClearVerifiedAt = true
	}
	acc.QuotaResetAt = existing.QuotaResetAt
	acc.MissingThinkingStrikes = existing.MissingThinkingStrikes
	acc.MissingThinkingLastAt = existing.MissingThinkingLastAt
	acc.GrokModels = append([]string(nil), existing.GrokModels...)
	acc.GrokModelsSyncedAt = existing.GrokModelsSyncedAt
	acc.GrokBilling = existing.GrokBilling
	acc.GrokRateLimits = existing.GrokRateLimits
	acc.GrokWebQuota = existing.GrokWebQuota
}

type accountOutput struct {
	*store.Account
	WarpAuthenticated bool `json:"warp_authenticated,omitempty"`
	// Quota holds the provider-specific quota projection. It is merged into every
	// account response so the management table can render 等级/配额 consistently
	// without re-deriving each channel's semantics on the client.
	Quota map[string]interface{} `json:"-"`
}

// MarshalJSON flattens the quota projection into the account object itself.
func (o accountOutput) MarshalJSON() ([]byte, error) {
	merged := map[string]interface{}{}
	if o.Account != nil {
		raw, err := json.Marshal(o.Account)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &merged); err != nil {
			return nil, err
		}
	}
	merged["warp_authenticated"] = o.WarpAuthenticated
	for key, value := range o.Quota {
		merged[key] = value
	}
	return json.Marshal(merged)
}

func normalizeAccountOutput(acc *store.Account) *accountOutput {
	out := normalizeWarpTokenOutput(acc)
	if out == nil {
		return nil
	}
	if strings.EqualFold(out.AccountType, "warp") && out.WarpMonthlyLimit > 0 {
		out.Subscription = warp.InferSubscriptionFromRequestLimit(&warp.RequestLimitInfo{
			RequestLimit: int(out.WarpMonthlyLimit),
		})
	}
	if strings.EqualFold(out.AccountType, "grok") {
		grok.NormalizeProvider(out)
		out.RefreshToken = ""
		out.SessionCookie = ""
		// The administrator explicitly opted in to seeing the short-lived OAuth
		// access token in the authenticated management UI. Never return the
		// durable refresh token through normal account endpoints.
		out.OAuthRefreshToken = ""
	}
	if strings.EqualFold(out.AccountType, "workbuddy") {
		// The durable refresh token never leaves the server; the access token
		// stays visible so the account table can prove a credential exists.
		out = RedactWorkBuddyOutput(out)
	}
	return &accountOutput{
		Account:           out,
		WarpAuthenticated: strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") && warp.RefreshToken(acc) != "",
		Quota:             buildQuotaResponseFields(out),
	}
}

func normalizedAccountCredentialKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}

	accountType := strings.ToLower(strings.TrimSpace(acc.AccountType))
	var token string

	switch accountType {
	case "warp":
		token = strings.TrimSpace(warp.RefreshToken(acc))
	case "grok":
		if grokAccountIsOAuth(acc) {
			token = strings.TrimSpace(util.FirstNonEmpty(acc.OAuthRefreshToken, acc.OAuthAccessToken))
		} else {
			token = grok.NormalizeSSOToken(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
		}
	case "puter":
		token = puter.ResolveAuthToken(acc)
	case "workbuddy":
		return WorkBuddyCredentialKey(acc)
	default:
		token = strings.TrimSpace(util.FirstNonEmpty(acc.RefreshToken, acc.SessionCookie, acc.ClientCookie, acc.Token))
	}

	if token == "" || accountType == "" {
		return ""
	}
	return accountType + ":" + token
}

func isSupportedAccountType(accountType string) bool {
	switch strings.ToLower(strings.TrimSpace(accountType)) {
	case "warp", "puter", "grok", "workbuddy":
		return true
	default:
		return false
	}
}

func (a *API) findDuplicateAccountByCredential(ctx context.Context, acc *store.Account, excludeID int64) (*store.Account, error) {
	if a == nil || a.store == nil || acc == nil {
		return nil, nil
	}

	key := normalizedAccountCredentialKey(acc)
	if key == "" {
		return nil, nil
	}

	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, existing := range accounts {
		if existing == nil || existing.ID == excludeID {
			continue
		}
		if normalizedAccountCredentialKey(existing) == key {
			if grokSSOViewsAreLinked(acc, existing) {
				continue
			}
			return existing, nil
		}
	}
	return nil, nil
}

func duplicateAccountError(existing *store.Account) error {
	if existing == nil {
		return fmt.Errorf("duplicate account token")
	}
	accountType := strings.TrimSpace(existing.AccountType)
	if accountType == "" {
		accountType = "account"
	}
	return fmt.Errorf("duplicate %s token already exists on account #%d", accountType, existing.ID)
}

func buildQuotaResponseFields(acc *store.Account) map[string]interface{} {
	fields := map[string]interface{}{
		"quota_limit":     0.0,
		"quota_used":      0.0,
		"quota_remaining": 0.0,
		"quota_mode":      "remaining",
		"quota_unit":      "credits",
		"quota_supported": true,
	}
	if acc == nil {
		return fields
	}

	limit := acc.UsageLimit
	current := acc.UsageCurrent
	if limit < 0 {
		limit = 0
	}
	if current < 0 {
		current = 0
	}

	switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
	case "workbuddy":
		// The meter reports the remaining credits of the current cycle; the
		// generic UsageCurrent slot stores that remaining value for this channel,
		// so "used" must be derived rather than read from UsageCurrent.
		snapshot := acc.WorkBuddyQuota
		quotaLimit := limit
		if snapshot.Limit > 0 {
			quotaLimit = snapshot.Limit
		}
		quotaRemaining := current
		if !snapshot.SyncedAt.IsZero() {
			quotaRemaining = snapshot.Remaining
		}
		if quotaLimit <= 0 {
			fields["quota_limit"] = 0.0
			fields["quota_used"] = 0.0
			fields["quota_remaining"] = 0.0
			fields["quota_mode"] = "unknown"
			fields["quota_unit"] = "credits"
			fields["quota_supported"] = false
			fields["quota_plan"] = snapshot.PackageName
			break
		}
		if quotaRemaining < 0 {
			quotaRemaining = 0
		}
		if quotaRemaining > quotaLimit {
			quotaRemaining = quotaLimit
		}
		used := snapshot.Used
		if snapshot.SyncedAt.IsZero() {
			used = quotaLimit - quotaRemaining
		}
		if used < 0 {
			used = 0
		}
		fields["quota_limit"] = quotaLimit
		fields["quota_used"] = used
		fields["quota_remaining"] = quotaRemaining
		fields["quota_mode"] = "remaining"
		fields["quota_unit"] = util.FirstNonEmpty(snapshot.Unit, "credits")
		fields["quota_supported"] = !snapshot.SyncedAt.IsZero()
		fields["quota_plan"] = snapshot.PackageName
		fields["quota_consumed_units"] = snapshot.LastConsumedUnits
		fields["quota_package_remaining"] = snapshot.PackageRemaining
		if !snapshot.ResyncAt().IsZero() {
			fields["quota_reset_at"] = snapshot.ResyncAt().UTC().Format(time.RFC3339)
		}
	case "grok":
		if grok.ProviderForAccount(acc) == grok.ProviderBuild {
			weekly := acc.GrokBilling.Weekly
			monthly := acc.GrokBilling.Monthly
			fields["quota_mode"] = "unknown"
			fields["quota_unit"] = "build_credits"
			fields["quota_supported"] = false
			if weekly.HasUsage {
				fields["quota_limit"] = 100.0
				fields["quota_used"] = weekly.UsagePercent
				fields["quota_remaining"] = max(0, 100-weekly.UsagePercent)
				fields["quota_mode"] = "weekly_percent"
				fields["quota_unit"] = "percent"
				fields["quota_supported"] = true
				fields["quota_reset_at"] = weekly.ResetAt
			}
			if monthly.HasLimit {
				fields["quota_monthly_limit"] = monthly.Limit
				fields["quota_monthly_remaining"] = monthly.Remaining
			}
			if acc.GrokRateLimits.Requests.HasLimit || acc.GrokRateLimits.Requests.HasRemaining {
				fields["rate_limit_requests"] = acc.GrokRateLimits.Requests
			}
			if acc.GrokRateLimits.Tokens.HasLimit || acc.GrokRateLimits.Tokens.HasRemaining {
				fields["rate_limit_tokens"] = acc.GrokRateLimits.Tokens
			}
			break
		}
		web := acc.GrokWebQuota
		preferredMode := ""
		preferred := web.Auto
		if !preferred.HasLimit && !preferred.HasRemaining {
			preferredMode = "fast"
			preferred = web.Fast
		} else {
			preferredMode = "auto"
		}
		if preferred.HasLimit || preferred.HasRemaining {
			limit = preferred.Limit
			remaining := preferred.Remaining
			used := limit - remaining
			if used < 0 {
				used = 0
			}
			fields["quota_limit"] = limit
			fields["quota_used"] = used
			fields["quota_remaining"] = remaining
			fields["quota_mode"] = "web_" + preferredMode
			fields["quota_unit"] = "requests"
			fields["quota_supported"] = true
			fields["quota_reset_at"] = preferred.ResetAt
			fields["quota_windows"] = map[string]interface{}{"auto": web.Auto, "fast": web.Fast}
		} else {
			// No successful Web quota snapshot is different from zero credits.
			// Keep the account active while telling the UI that the value is
			// currently unavailable instead of inventing a default allowance.
			fields["quota_limit"] = 0.0
			fields["quota_used"] = 0.0
			fields["quota_remaining"] = 0.0
			fields["quota_mode"] = "unavailable"
			fields["quota_unit"] = "requests"
			fields["quota_supported"] = false
		}
	case "warp":
		baseLimit := limit
		if acc.WarpMonthlyLimit > 0 {
			baseLimit = acc.WarpMonthlyLimit
		}
		used := current
		if used > baseLimit && baseLimit > 0 {
			used = baseLimit
		}
		baseRemaining := acc.WarpMonthlyRemaining
		if baseRemaining <= 0 && baseLimit > 0 {
			baseRemaining = baseLimit - current
		}
		if baseRemaining < 0 {
			baseRemaining = 0
		}
		bonusRemaining := acc.WarpBonusRemaining
		if bonusRemaining < 0 {
			bonusRemaining = 0
		}
		remaining := baseRemaining + bonusRemaining
		fields["quota_limit"] = baseLimit
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
		fields["quota_mode"] = "warp_split"
		fields["quota_unit"] = "requests"
		fields["quota_base_limit"] = baseLimit
		fields["quota_base_remaining"] = baseRemaining
		fields["quota_bonus_remaining"] = bonusRemaining
	case "puter":
		if limit <= 0 {
			fields["quota_limit"] = 0.0
			fields["quota_used"] = 0.0
			fields["quota_remaining"] = 0.0
			fields["quota_mode"] = "unknown"
			fields["quota_unit"] = "credits"
			fields["quota_supported"] = false
			break
		}
		remaining := current
		if remaining > limit {
			remaining = limit
		}
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
		fields["quota_mode"] = "remaining"
		fields["quota_unit"] = "credits"
	default:
		fields["quota_limit"] = limit
		remaining := current
		if remaining > limit && limit > 0 {
			remaining = limit
		}
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		fields["quota_used"] = used
		fields["quota_remaining"] = remaining
	}

	return fields
}

func applyPuterMonthlyUsage(acc *store.Account, usage *puter.MonthlyUsage) {
	if acc == nil || usage == nil {
		return
	}
	limit := usage.AllowanceInfo.MonthUsageAllowance
	remaining := usage.AllowanceInfo.Remaining
	if limit < 0 {
		limit = 0
	}
	if remaining < 0 {
		remaining = 0
	}
	if limit > 0 && remaining > limit {
		remaining = limit
	}
	acc.UsageCurrent = remaining
	acc.UsageLimit = limit
}

func (a *API) refreshAccountState(ctx context.Context, acc *store.Account) (string, int, error) {
	if acc == nil {
		return "", http.StatusBadRequest, fmt.Errorf("account is nil")
	}

	if strings.EqualFold(acc.AccountType, "warp") {
		cfg := a.config.Load()
		warpClient := warp.NewFromAccount(acc, cfg)
		_, err := warpClient.ForceRefreshAccount(ctx)
		if err != nil {
			httpStatus := http.StatusBadRequest
			if code := warp.HTTPStatusCode(err); code >= 400 {
				httpStatus = code
			}
			accountStatus := ""
			if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden || httpStatus == http.StatusTooManyRequests {
				accountStatus = strconv.Itoa(httpStatus)
			}
			return accountStatus, httpStatus, fmt.Errorf("failed to refresh warp account: %w", err)
		}
		warpClient.SyncAccountState()

		limitCtx, limitCancel := context.WithTimeout(ctx, 15*time.Second)
		limitInfo, bonuses, limitErr := warpClient.GetRequestLimitInfo(limitCtx)
		limitCancel()
		if limitErr == nil && limitInfo != nil {
			warp.ApplyRequestLimitInfoToAccount(acc, limitInfo, bonuses)
		} else if limitErr != nil {
			slog.Warn("Warp quota sync failed after refresh; keeping account available", "account_id", acc.ID, "error", limitErr)
		}
		modelDiscoveryConfirmed := false
		var modelDiscoveryErr error
		if a.store != nil && acc.ID != 0 {
			modelCtx, modelCancel := context.WithTimeout(ctx, 15*time.Second)
			features, source, modelErr := warpClient.FetchDiscoveredFeatureModelChoices(modelCtx)
			modelCancel()
			choices := warp.AgentModeModelChoices(features)
			featureConfig := warp.AccountFeatureConfigFromChoices(features)
			if modelErr == nil && len(choices) > 0 {
				modelDiscoveryConfirmed = true
				models := make([]string, 0, len(choices))
				for _, choice := range choices {
					models = append(models, choice.ID)
				}
				existing, err := warp.LoadAccountModelChoices(ctx, a.store)
				if err != nil {
					slog.Warn("Warp model choices sync failed after refresh", "account_id", acc.ID, "source", source, "error", err)
				} else {
					if existing == nil {
						existing = &warp.AccountModelChoices{Accounts: map[string][]string{}}
					}
					if existing.Accounts == nil {
						existing.Accounts = map[string][]string{}
					}
					if existing.Sources == nil {
						existing.Sources = map[string]string{}
					}
					if existing.FeatureConfigs == nil {
						existing.FeatureConfigs = map[string]warp.AccountFeatureConfig{}
					}
					key := strconv.FormatInt(acc.ID, 10)
					existing.Accounts[key] = models
					existing.Sources[key] = source
					if !featureConfig.IsEmpty() {
						existing.FeatureConfigs[key] = featureConfig
					}
					if err := warp.SaveAccountModelChoices(ctx, a.store, existing); err != nil {
						slog.Warn("Warp model choices sync failed after refresh", "account_id", acc.ID, "source", source, "error", err)
					}
				}
			} else if modelErr != nil {
				modelDiscoveryErr = modelErr
				slog.Warn("Warp model choices fetch failed after refresh", "account_id", acc.ID, "error", modelErr)
			} else {
				modelDiscoveryErr = fmt.Errorf("warp model discovery returned no enabled models")
			}
		}
		if strings.TrimSpace(acc.StatusCode) == "403" {
			if !modelDiscoveryConfirmed {
				if modelDiscoveryErr == nil {
					modelDiscoveryErr = fmt.Errorf("warp model discovery unavailable")
				}
				return "403", http.StatusForbidden, fmt.Errorf("failed to verify warp AI entitlement without a billable probe: %w", modelDiscoveryErr)
			}
		}
		return "", 0, nil
	}

	if strings.EqualFold(acc.AccountType, "grok") {
		if verifyErr := verifyGrokAccount(ctx, acc, a.config.Load(), a.store); verifyErr != nil {
			message := strings.ToLower(verifyErr.Error())
			if strings.Contains(message, "missing sso token") || strings.Contains(message, "missing oauth token") {
				return "", http.StatusBadRequest, fmt.Errorf("failed to verify grok account: %w", verifyErr)
			}
			status := apperrors.ClassifyAccountStatus(verifyErr.Error())
			return status, httpStatusFromAccountStatus(status), fmt.Errorf("failed to verify grok account: %w", verifyErr)
		}
		return "", 0, nil
	}

	if strings.EqualFold(acc.AccountType, "puter") {
		if puter.ResolveAuthToken(acc) == "" {
			return "", http.StatusBadRequest, fmt.Errorf("failed to verify puter account: missing auth token")
		}
		usage, usageErr := puterFetchMonthlyUsage(ctx, acc, a.config.Load())
		if usageErr == nil {
			applyPuterMonthlyUsage(acc, usage)
			if acc.UsageLimit > 0 && acc.UsageCurrent <= 0 {
				return "402", 0, nil
			}
			return "", 0, nil
		}
		usageStatus := apperrors.ClassifyAccountStatus(usageErr.Error())
		httpStatus := http.StatusBadGateway
		if usageStatus != "" {
			httpStatus = httpStatusFromAccountStatus(usageStatus)
		}
		return usageStatus, httpStatus, fmt.Errorf("failed to fetch puter usage: %w", usageErr)
	}

	if strings.EqualFold(acc.AccountType, "workbuddy") {
		status, httpStatus, verifyErr := verifyWorkBuddyAccount(ctx, acc, a.config.Load())
		if verifyErr != nil {
			if errors.Is(verifyErr, errWorkBuddyMissingCredential) {
				return "", http.StatusBadRequest, fmt.Errorf("failed to verify workbuddy account: %w", verifyErr)
			}
			if classified := apperrors.ClassifyAccountStatus(verifyErr.Error()); classified != "" {
				return classified, httpStatusFromAccountStatus(classified), fmt.Errorf("failed to verify workbuddy account: %w", verifyErr)
			}
			return status, httpStatus, fmt.Errorf("failed to verify workbuddy account: %w", verifyErr)
		}
		return status, httpStatus, nil
	}

	return "", http.StatusBadRequest, fmt.Errorf("unsupported account type %q", acc.AccountType)
}

type ExportData struct {
	Version  int             `json:"version"`
	ExportAt time.Time       `json:"export_at"`
	Accounts []store.Account `json:"accounts"`
}

type ImportResult struct {
	Total    int `json:"total"`
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

type CreateKeyResponse struct {
	ID            int64      `json:"id"`
	Key           string     `json:"key"`
	Name          string     `json:"name"`
	KeyPrefix     string     `json:"key_prefix"`
	KeySuffix     string     `json:"key_suffix"`
	Enabled       bool       `json:"enabled"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	RPMLimit      int        `json:"rpm_limit,omitempty"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type UpdateKeyRequest struct {
	Enabled       *bool           `json:"enabled"`
	AllowedModels *[]string       `json:"allowed_models"`
	RPMLimit      *int            `json:"rpm_limit"`
	MaxConcurrent *int            `json:"max_concurrent"`
	ExpiresAt     json.RawMessage `json:"expires_at"`
}

func normalizeAllowedModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if model == "*" {
			return []string{"*"}
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	return normalized
}

func parseOptionalExpiry(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var expiresAt time.Time
	if err := json.Unmarshal(raw, &expiresAt); err != nil {
		return nil, fmt.Errorf("expires_at must be an RFC3339 timestamp or null")
	}
	expiresAt = expiresAt.UTC()
	return &expiresAt, nil
}

func New(s *store.Store, adminUser, adminPass string, cfg *config.Config) *API {
	a := &API{
		store:        s,
		adminUser:    adminUser,
		adminPass:    adminPass,
		loginLimiter: middleware.NewRateLimiter(5, 15*time.Minute),

		checkInFlight:    map[int64]bool{},
		checkFailCount:   map[int64]int{},
		checkNextAllowed: map[int64]time.Time{},
		checkSem:         make(chan struct{}, 2),
		warpDeviceLogins: map[string]*warpDeviceLogin{},
		grokDeviceLogins: map[string]*grokDeviceLogin{},
		workbuddyLogins:  map[string]*workbuddyLogin{},
	}
	if cfg != nil {
		a.config.Store(cfg)
	}
	return a
}

func (a *API) SetPromptCache(cache tokencache.PromptCache) {
	a.promptCache = cache
}

func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := middleware.ClientIP(r)
	if a.loginLimiter != nil && !a.loginLimiter.Allow(ip) {
		http.Error(w, "Too many login attempts, try again later", http.StatusTooManyRequests)
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	adminUser := a.adminUser
	adminPass := a.adminPass
	if cfg := a.config.Load(); cfg != nil {
		adminUser = cfg.AdminUser
		adminPass = cfg.AdminPass
	}

	if !util.SecureCompare(req.Username, adminUser) || !util.SecureCompare(req.Password, adminPass) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := auth.GenerateSessionToken()
	if err != nil {
		slog.Error("Failed to generate session token", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// NOTE: Do not mark cookies as Secure when served over plain HTTP,
	// otherwise browsers will drop the cookie and the Admin UI will appear unable to log in.
	// When behind a TLS-terminating proxy, honor X-Forwarded-Proto.
	isHTTPS := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_token")
	if err == nil {
		auth.InvalidateSessionToken(cookie.Value)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session_token",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (a *API) HandleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(a.config.Load())
	case http.MethodPost:
		// Copy current config, decode into copy, then atomically store
		current := a.config.Load()
		newCfg := *current
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.persistConfig(r.Context(), current, &newCfg); err != nil {
			http.Error(w, "Failed to save config: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(&newCfg)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleConfigList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	data, err := configPayload(a.config.Load())
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "获取配置失败: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"data": data,
	})
}

func (a *API) HandleConfigSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	current := a.config.Load()
	newCfg, err := buildConfigFromPatch(r, current)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "parse request failed: " + err.Error(),
		})
		return
	}
	if err := a.persistConfig(r.Context(), current, newCfg); err != nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"msg":  "save config failed: " + err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"msg":  "success",
	})
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		accounts, err := a.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if accounts == nil {
			accounts = []*store.Account{}
		}
		normalized := make([]*accountOutput, 0, len(accounts))
		for _, acc := range accounts {
			if acc == nil || isLinkedGrokConsoleAccount(acc) {
				continue
			}
			normalized = append(normalized, normalizeAccountOutput(acc))
		}
		json.NewEncoder(w).Encode(normalized)

	case http.MethodPost:
		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		acc.GrokSSOParentID = 0
		if strings.TrimSpace(acc.AccountType) == "" {
			http.Error(w, "account_type is required", http.StatusBadRequest)
			return
		}
		if !isSupportedAccountType(acc.AccountType) {
			http.Error(w, "unsupported account type", http.StatusBadRequest)
			return
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			http.Error(w, "Warp accounts must be added using official web login (/api/warp/device-auth)", http.StatusBadRequest)
			return
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			if !grokAccountIsOAuth(&acc) {
				acc.GrokProvider = grok.ProviderWeb
			}
			acc.NSFWEnabled = true
			if grokAccountIsOAuth(&acc) && !grokAccountHasOAuthCredentials(&acc) {
				http.Error(w, "missing oauth token", http.StatusBadRequest)
				return
			}
			if !grokAccountIsOAuth(&acc) && grok.NormalizeSSOToken(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token)) == "" {
				http.Error(w, "missing sso token", http.StatusBadRequest)
				return
			}
			if !grokAccountIsOAuth(&acc) {
				if err := a.reconcileGrokSSOProviderCredential(r.Context(), &acc); err != nil {
					slog.Error("Failed to repair linked Grok Console SSO account before create", "error", err)
					http.Error(w, "failed to repair linked Grok Console SSO account", http.StatusInternalServerError)
					return
				}
			}
		} else if strings.EqualFold(acc.AccountType, "workbuddy") {
			if !NormalizeWorkBuddyCredentials(&acc) {
				http.Error(w, "missing WorkBuddy credential: paste the access token or refresh token from the WorkBuddy desktop session", http.StatusBadRequest)
				return
			}
		}
		if existing, err := a.findDuplicateAccountByCredential(r.Context(), &acc, 0); err != nil {
			slog.Error("Failed to detect duplicate account token", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if existing != nil {
			http.Error(w, duplicateAccountError(existing).Error(), http.StatusConflict)
			return
		}

		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Error("Failed to create account", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.syncGrokSSOProviderView(r.Context(), &acc); err != nil {
			slog.Error("Failed to create linked Grok Console SSO account", "account_id", acc.ID, "error", err)
			if cleanupErr := a.deleteGrokSSOSourceAndLinkedConsoleAccounts(r.Context(), acc.ID); cleanupErr != nil {
				slog.Error("Failed to compensate incomplete Grok SSO pair creation", "account_id", acc.ID, "error", cleanupErr)
			}
			http.Error(w, "failed to create linked Grok Console SSO account", http.StatusInternalServerError)
			return
		}

		if acc.Enabled {
			if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Account-Sync")), "async") {
				a.syncAccountAfterCreate(acc)
			} else {
				syncCtx, syncCancel := context.WithTimeout(r.Context(), 25*time.Second)
				accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &acc)
				syncCancel()
				if syncErr != nil {
					slog.Warn("Initial account sync failed", "account_id", acc.ID, "type", acc.AccountType, "error", syncErr)
					if accountStatus != "" {
						acc.StatusCode = accountStatus
						acc.StatusMessage = strings.TrimSpace(syncErr.Error())
						acc.LastAttempt = time.Now()
						acc.VerifiedAt = acc.LastAttempt
					}
				} else {
					applySuccessfulAccountRefreshStatus(&acc, accountStatus)
				}
				// A credential the upstream definitively rejects must not be
				// persisted as a healthy account: it would sit in the pool looking
				// 正常 while every request routed to it fails.
				if acc.StatusCode == "401" {
					if deleteErr := a.store.DeleteAccount(r.Context(), acc.ID); deleteErr != nil {
						slog.Error("Failed to roll back rejected account", "account_id", acc.ID, "type", acc.AccountType, "error", deleteErr)
					} else {
						slog.Warn("Rejected account was not saved (upstream refused the credential)",
							"account_id", acc.ID, "type", acc.AccountType, "reason", acc.StatusMessage)
					}
					apperrors.New("authentication_error",
						"account was rejected by the upstream and was not saved: "+strings.TrimSpace(acc.StatusMessage),
						http.StatusUnauthorized).WriteResponse(w)
					return
				}
				if updateErr := a.store.UpdateAccount(r.Context(), &acc); updateErr != nil {
					slog.Warn("Failed to persist initial account sync", "account_id", acc.ID, "type", acc.AccountType, "error", updateErr)
				}
			}
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleWarpDeviceAuthorization starts and observes the official Warp Agent
// CLI device-authorization flow. It is registered behind the admin session
// middleware; no Warp credentials are accepted from or returned to the UI.
func (a *API) HandleWarpDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/warp/device-auth")
	path = strings.Trim(path, "/")

	switch {
	case r.Method == http.MethodPost && path == "":
		a.startWarpDeviceAuthorization(w, r)
	case r.Method == http.MethodGet && path != "":
		a.getWarpDeviceAuthorization(w, r, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelWarpDeviceAuthorization(w, r, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startWarpDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		http.Error(w, "account store is not configured", http.StatusServiceUnavailable)
		return
	}
	a.cleanupWarpDeviceLogins(time.Now())

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	authenticator := warp.NewDeviceAuthenticator(a.config.Load())
	details, err := authenticator.Start(ctx)
	if err != nil {
		slog.Warn("Warp device authorization could not be started", "error", err)
		http.Error(w, "failed to start Warp device authorization", http.StatusBadGateway)
		return
	}

	id, err := newDeviceLoginID()
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	expiresAt := time.Now().Add(time.Duration(details.ExpiresIn) * time.Second)
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &warpDeviceLogin{
		deviceCode: details.DeviceCode,
		userCode:   details.UserCode,
		verifyURI:  details.VerificationURI,
		verifyFull: details.VerificationURIComplete,
		expiresAt:  expiresAt,
		interval:   time.Duration(details.Interval) * time.Second,
		cancel:     pollCancel,
		status:     "pending",
	}

	a.warpDeviceLoginMu.Lock()
	if len(a.warpDeviceLogins) >= maxDeviceLogins {
		a.warpDeviceLoginMu.Unlock()
		pollCancel()
		http.Error(w, "too many pending Warp device logins", http.StatusTooManyRequests)
		return
	}
	a.warpDeviceLogins[id] = login
	a.warpDeviceLoginMu.Unlock()

	go a.pollWarpDeviceAuthorization(pollContext, id, authenticator)
	json.NewEncoder(w).Encode(newDeviceLoginResponse(id, login))
}

func (a *API) getWarpDeviceAuthorization(w http.ResponseWriter, _ *http.Request, id string) {
	a.cleanupWarpDeviceLogins(time.Now())
	a.warpDeviceLoginMu.Lock()
	login := a.warpDeviceLogins[id]
	if login == nil {
		a.warpDeviceLoginMu.Unlock()
		http.Error(w, "Warp device login not found", http.StatusNotFound)
		return
	}
	response := newDeviceLoginResponse(id, login)
	a.warpDeviceLoginMu.Unlock()
	json.NewEncoder(w).Encode(response)
}

func (a *API) cancelWarpDeviceAuthorization(w http.ResponseWriter, _ *http.Request, id string) {
	a.warpDeviceLoginMu.Lock()
	login := a.warpDeviceLogins[id]
	if login == nil {
		a.warpDeviceLoginMu.Unlock()
		http.Error(w, "Warp device login not found", http.StatusNotFound)
		return
	}
	delete(a.warpDeviceLogins, id)
	login.deviceCode = ""
	login.status = "cancelled"
	if login.cancel != nil {
		login.cancel()
	}
	a.warpDeviceLoginMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) pollWarpDeviceAuthorization(ctx context.Context, id string, authenticator *warp.DeviceAuthenticator) {
	for {
		login, ok := a.warpDeviceLoginForPoll(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.finishWarpDeviceLogin(id, "expired", "Warp authorization expired", 0)
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
		}

		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		refreshToken, err := authenticator.Exchange(requestCtx, login.deviceCode)
		cancel()
		if err != nil {
			if warp.IsDeviceAuthorizationPending(err) {
				continue
			}
			slog.Warn("Warp device authorization failed", "login_id", id, "error", err)
			a.finishWarpDeviceLogin(id, "failed", "Warp authorization failed", 0)
			return
		}

		acc := &store.Account{
			Name:         "warp-device-login",
			AccountType:  "warp",
			RefreshToken: refreshToken,
			Weight:       1,
			Enabled:      true,
		}
		normalizeWarpTokenInput(acc)
		storeCtx, storeCancel := context.WithTimeout(ctx, 20*time.Second)
		existing, err := a.findDuplicateAccountByCredential(storeCtx, acc, 0)
		if err == nil && existing == nil {
			err = a.store.CreateAccount(storeCtx, acc)
		}
		storeCancel()
		if err != nil {
			slog.Warn("Warp device authorization could not save account", "login_id", id, "error", err)
			a.finishWarpDeviceLogin(id, "failed", "Warp authorization succeeded but account could not be saved", 0)
			return
		}
		if existing != nil {
			a.finishWarpDeviceLogin(id, "complete", "Warp account already exists", existing.ID)
			return
		}

		a.finishWarpDeviceLogin(id, "complete", "Warp account added", acc.ID)
		a.syncAccountAfterCreate(*acc)
		return
	}
}

func (a *API) warpDeviceLoginForPoll(id string) (*warpDeviceLogin, bool) {
	a.warpDeviceLoginMu.Lock()
	defer a.warpDeviceLoginMu.Unlock()
	return deviceLoginForPoll(a.warpDeviceLogins, id)
}

func (a *API) finishWarpDeviceLogin(id, status, message string, accountID int64) {
	a.warpDeviceLoginMu.Lock()
	defer a.warpDeviceLoginMu.Unlock()
	finishDeviceLogin(a.warpDeviceLogins[id], status, message, accountID)
}

func (a *API) cleanupWarpDeviceLogins(now time.Time) {
	if a == nil {
		return
	}
	a.warpDeviceLoginMu.Lock()
	defer a.warpDeviceLoginMu.Unlock()
	cleanupDeviceLogins(a.warpDeviceLogins, now, "Warp authorization expired")
}

func deviceLoginForPoll(logins map[string]*deviceLogin, id string) (*deviceLogin, bool) {
	login := logins[id]
	if login == nil || login.status != "pending" || strings.TrimSpace(login.deviceCode) == "" {
		return nil, false
	}
	copyLogin := *login
	return &copyLogin, true
}

func finishDeviceLogin(login *deviceLogin, status, message string, accountID int64) {
	if login == nil {
		return
	}
	login.deviceCode = ""
	login.userCode = ""
	login.verifyURI = ""
	login.verifyFull = ""
	login.status = status
	login.message = message
	login.accountID = accountID
	if login.cancel != nil {
		login.cancel()
	}
}

func cleanupDeviceLogins(logins map[string]*deviceLogin, now time.Time, expiredMessage string) {
	for id, login := range logins {
		if login == nil {
			delete(logins, id)
			continue
		}
		if login.status == "pending" && now.After(login.expiresAt) {
			finishDeviceLogin(login, "expired", expiredMessage, 0)
		}
		if now.After(login.expiresAt.Add(15 * time.Minute)) {
			delete(logins, id)
		}
	}
}

func newDeviceLoginResponse(id string, login *deviceLogin) deviceLoginResponse {
	response := deviceLoginResponse{ID: id}
	if login == nil {
		return response
	}
	response.Status = login.status
	response.Message = login.message
	response.AccountID = login.accountID
	if login.status == "pending" {
		response.UserCode = login.userCode
		response.VerificationURI = login.verifyURI
		response.VerificationURIComplete = login.verifyFull
		response.ExpiresAt = login.expiresAt.UTC().Format(time.RFC3339)
	}
	return response
}

func newDeviceLoginID() (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// HandleGrokDeviceAuthorization starts and observes the official xAI Grok
// Build CLI device-authorization flow. It accepts no files, browser cookies,
// passwords, or user-supplied tokens; device codes remain server-side only.
func (a *API) HandleGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/grok/device-auth"), "/")
	switch {
	case r.Method == http.MethodPost && path == "":
		a.startGrokDeviceAuthorization(w, r)
	case r.Method == http.MethodGet && path != "":
		a.getGrokDeviceAuthorization(w, path)
	case r.Method == http.MethodDelete && path != "":
		a.cancelGrokDeviceAuthorization(w, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) startGrokDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if a == nil || a.store == nil {
		http.Error(w, "account store is not configured", http.StatusServiceUnavailable)
		return
	}
	a.cleanupGrokDeviceLogins(time.Now())
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	authenticator := grok.NewDeviceAuthenticator(a.config.Load())
	details, err := authenticator.Start(ctx)
	if err != nil {
		slog.Warn("Grok device authorization could not be started", "error", err)
		http.Error(w, "failed to start Grok device authorization", http.StatusBadGateway)
		return
	}
	id, err := newDeviceLoginID()
	if err != nil {
		http.Error(w, "failed to create login transaction", http.StatusInternalServerError)
		return
	}
	pollContext, pollCancel := context.WithCancel(context.Background())
	login := &grokDeviceLogin{
		deviceCode: details.DeviceCode,
		userCode:   details.UserCode,
		verifyURI:  details.VerificationURI,
		verifyFull: details.VerificationURIComplete,
		expiresAt:  time.Now().Add(time.Duration(details.ExpiresIn) * time.Second),
		interval:   time.Duration(details.Interval) * time.Second,
		cancel:     pollCancel,
		status:     "pending",
	}
	a.grokDeviceLoginMu.Lock()
	if len(a.grokDeviceLogins) >= maxDeviceLogins {
		a.grokDeviceLoginMu.Unlock()
		pollCancel()
		http.Error(w, "too many pending Grok device logins", http.StatusTooManyRequests)
		return
	}
	a.grokDeviceLogins[id] = login
	a.grokDeviceLoginMu.Unlock()
	go a.pollGrokDeviceAuthorization(pollContext, id, authenticator)
	json.NewEncoder(w).Encode(newDeviceLoginResponse(id, login))
}

func (a *API) getGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	a.cleanupGrokDeviceLogins(time.Now())
	a.grokDeviceLoginMu.Lock()
	login := a.grokDeviceLogins[id]
	if login == nil {
		a.grokDeviceLoginMu.Unlock()
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	response := newDeviceLoginResponse(id, login)
	a.grokDeviceLoginMu.Unlock()
	json.NewEncoder(w).Encode(response)
}

func (a *API) cancelGrokDeviceAuthorization(w http.ResponseWriter, id string) {
	a.grokDeviceLoginMu.Lock()
	login := a.grokDeviceLogins[id]
	if login == nil {
		a.grokDeviceLoginMu.Unlock()
		http.Error(w, "Grok device login not found", http.StatusNotFound)
		return
	}
	delete(a.grokDeviceLogins, id)
	login.deviceCode = ""
	login.status = "cancelled"
	if login.cancel != nil {
		login.cancel()
	}
	a.grokDeviceLoginMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) pollGrokDeviceAuthorization(ctx context.Context, id string, authenticator *grok.DeviceAuthenticator) {
	for {
		login, ok := a.grokDeviceLoginForPoll(id)
		if !ok {
			return
		}
		if time.Now().After(login.expiresAt) {
			a.finishGrokDeviceLogin(id, "expired", "Grok authorization expired", 0)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(login.interval):
		}
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		accessToken, refreshToken, expiresAt, err := authenticator.Exchange(requestCtx, login.deviceCode)
		cancel()
		if err != nil {
			if slowDown, pending := grok.IsDeviceAuthorizationPending(err); pending {
				if slowDown {
					a.increaseGrokDeviceLoginInterval(id)
				}
				continue
			}
			slog.Warn("Grok device authorization failed", "login_id", id, "error", err)
			a.finishGrokDeviceLogin(id, "failed", "Grok authorization failed", 0)
			return
		}
		acc := &store.Account{
			Name:              "grok-device-login",
			AccountType:       "grok",
			CredentialType:    "oauth",
			OAuthAccessToken:  accessToken,
			OAuthRefreshToken: refreshToken,
			OAuthExpiresAt:    expiresAt,
			AgentMode:         "grok-build-0.1",
			Weight:            1,
			Enabled:           true,
			NSFWEnabled:       true,
		}
		grok.ApplyCLIOAuthIdentity(acc)
		normalizeGrokTokenInput(acc)
		storeCtx, storeCancel := context.WithTimeout(ctx, 20*time.Second)
		existing, err := a.findDuplicateAccountByCredential(storeCtx, acc, 0)
		if err == nil && existing == nil {
			err = a.store.CreateAccount(storeCtx, acc)
		}
		storeCancel()
		if err != nil {
			slog.Warn("Grok device authorization could not save account", "login_id", id, "error", err)
			a.finishGrokDeviceLogin(id, "failed", "Grok authorization succeeded but account could not be saved", 0)
			return
		}
		if existing != nil {
			a.finishGrokDeviceLogin(id, "complete", "Grok account already exists", existing.ID)
			return
		}
		a.finishGrokDeviceLogin(id, "complete", "Grok account added", acc.ID)
		a.syncAccountAfterCreate(*acc)
		return
	}
}

func (a *API) grokDeviceLoginForPoll(id string) (*grokDeviceLogin, bool) {
	a.grokDeviceLoginMu.Lock()
	defer a.grokDeviceLoginMu.Unlock()
	return deviceLoginForPoll(a.grokDeviceLogins, id)
}

func (a *API) increaseGrokDeviceLoginInterval(id string) {
	a.grokDeviceLoginMu.Lock()
	defer a.grokDeviceLoginMu.Unlock()
	if login := a.grokDeviceLogins[id]; login != nil && login.status == "pending" {
		login.interval += 5 * time.Second
	}
}

func (a *API) finishGrokDeviceLogin(id, status, message string, accountID int64) {
	a.grokDeviceLoginMu.Lock()
	defer a.grokDeviceLoginMu.Unlock()
	finishDeviceLogin(a.grokDeviceLogins[id], status, message, accountID)
}

func (a *API) cleanupGrokDeviceLogins(now time.Time) {
	if a == nil {
		return
	}
	a.grokDeviceLoginMu.Lock()
	defer a.grokDeviceLoginMu.Unlock()
	cleanupDeviceLogins(a.grokDeviceLogins, now, "Grok authorization expired")
}

func (a *API) HandleAccountByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(path, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	account, err := a.store.GetAccount(r.Context(), id)
	if err != nil || isLinkedGrokConsoleAccount(account) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	isRefresh := len(parts) > 1 && parts[1] == "refresh"
	isVerify := len(parts) > 1 && parts[1] == "verify"
	isCheck := len(parts) > 1 && parts[1] == "check"
	isUsage := len(parts) > 1 && parts[1] == "usage"
	if len(parts) > 2 || (len(parts) > 1 && !(isRefresh || isVerify || isCheck || isUsage)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if isRefresh || isVerify {
			http.Error(w, "Deprecated endpoint. Use /api/accounts/{id}/check instead.", http.StatusGone)
			return
		}
		if isUsage {
			resp := map[string]interface{}{
				"account_id":     account.ID,
				"name":           account.Name,
				"account_type":   account.AccountType,
				"subscription":   account.Subscription,
				"usage_current":  account.UsageCurrent,
				"usage_limit":    account.UsageLimit,
				"usage_total":    account.UsageTotal,
				"quota_reset_at": account.QuotaResetAt,
			}
			for k, v := range buildQuotaResponseFields(account) {
				resp[k] = v
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		if isCheck {
			// Storm control / backoff: only allow a small number of concurrent checks,
			// and apply exponential backoff per account on failures.
			now := time.Now()
			a.checkMu.Lock()
			if a.checkInFlight[id] {
				a.checkMu.Unlock()
				http.Error(w, "account check already in progress", http.StatusTooManyRequests)
				return
			}
			if next, ok := a.checkNextAllowed[id]; ok && !next.IsZero() && now.Before(next) {
				retryAfter := int(next.Sub(now).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				a.checkMu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "account check backoff", http.StatusTooManyRequests)
				return
			}
			a.checkInFlight[id] = true
			a.checkMu.Unlock()
			defer func() {
				a.checkMu.Lock()
				delete(a.checkInFlight, id)
				a.checkMu.Unlock()
			}()

			// global concurrency limit
			a.checkSem <- struct{}{}
			defer func() { <-a.checkSem }()

			acc := account
			checkOK := false
			checkErrStatus := ""
			defer func() {
				a.checkMu.Lock()
				defer a.checkMu.Unlock()
				if checkOK {
					a.checkFailCount[id] = 0
					a.checkNextAllowed[id] = time.Now().Add(3 * time.Second)
					return
				}
				fails := a.checkFailCount[id] + 1
				a.checkFailCount[id] = fails
				d := time.Duration(1<<min(fails, 8)) * time.Second
				// For CF/rate-limit style failures, start with a bigger cooldown.
				if checkErrStatus == "403" || checkErrStatus == "429" {
					if d < 60*time.Second {
						d = 60 * time.Second
					}
				}
				if d > 10*time.Minute {
					d = 10 * time.Minute
				}
				a.checkNextAllowed[id] = time.Now().Add(d)
			}()

			accountStatus, httpStatus, refreshErr := a.refreshAccountState(r.Context(), acc)
			if refreshErr != nil {
				checkErrStatus = accountStatus
				if accountStatus != "" {
					acc.StatusCode = accountStatus
					// The reason matters: a bare "401" cannot tell an operator
					// whether the credential was retired upstream or the record
					// lost it.
					acc.StatusMessage = strings.TrimSpace(refreshErr.Error())
					acc.LastAttempt = time.Now()
					acc.VerifiedAt = acc.LastAttempt
					if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
						slog.Warn("Failed to persist account refresh status", "account_id", acc.ID, "error", updateErr)
					}
				}
				if httpStatus == 0 {
					httpStatus = http.StatusBadRequest
				}
				http.Error(w, refreshErr.Error(), httpStatus)
				return
			}

			// 刷新/验证成功后清理账号状态
			applySuccessfulAccountRefreshStatus(acc, accountStatus)
			checkOK = true

			if err := a.store.UpdateAccount(r.Context(), acc); err != nil {
				http.Error(w, "Failed to save checked account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(normalizeAccountOutput(acc))
			return
		}
		json.NewEncoder(w).Encode(normalizeAccountOutput(account))

	case http.MethodPut:
		existing := account

		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.ID = id
		acc.GrokSSOParentID = existing.GrokSSOParentID
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = existing.AccountType
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if strings.EqualFold(strings.TrimSpace(existing.AccountType), "warp") && acc.AccountType != "warp" {
			http.Error(w, "Warp login accounts cannot change account type", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(acc.AccountType) == "" {
			http.Error(w, "account_type is required", http.StatusBadRequest)
			return
		}
		if !isSupportedAccountType(acc.AccountType) {
			http.Error(w, "unsupported account type", http.StatusBadRequest)
			return
		}
		if isGrokSSOAccount(existing) && existing.GrokSSOParentID == 0 && grok.ProviderForAccount(existing) == grok.ProviderWeb {
			hasChild, err := a.hasLinkedGrokConsoleCompanion(r.Context(), existing.ID)
			if err != nil {
				http.Error(w, "failed to inspect linked Grok Console account", http.StatusInternalServerError)
				return
			}
			if hasChild && acc.AccountType != "grok" {
				http.Error(w, "linked Grok Web SSO sources cannot change account type; delete the source before creating a replacement", http.StatusConflict)
				return
			}
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			if !strings.EqualFold(strings.TrimSpace(existing.AccountType), "warp") ||
				acc.RefreshToken != "" || acc.Token != "" || acc.ClientCookie != "" ||
				acc.SessionCookie != "" || acc.OAuthAccessToken != "" || acc.OAuthRefreshToken != "" {
				http.Error(w, "Warp credentials can only be obtained through official web login", http.StatusBadRequest)
				return
			}
			acc.RefreshToken = existing.RefreshToken
			normalizeWarpTokenInput(&acc)
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			if grokAccountIsOAuth(&acc) && isGrokSSOAccount(existing) && existing.GrokSSOParentID == 0 && grok.ProviderForAccount(existing) == grok.ProviderWeb {
				hasChild, err := a.hasLinkedGrokConsoleCompanion(r.Context(), existing.ID)
				if err != nil {
					http.Error(w, "failed to inspect linked Grok Console account", http.StatusInternalServerError)
					return
				}
				if hasChild {
					http.Error(w, "linked Grok Web SSO sources cannot change credential mode; delete the source before creating a replacement", http.StatusConflict)
					return
				}
			}
			if !grokAccountIsOAuth(&acc) {
				// Editing an SSO source keeps it on its Web provider.
				acc.GrokProvider = grok.ProviderWeb
			}
			// Admin UI redacts OAuth secrets on read; empty inbound fields mean
			// "keep existing", not "clear credentials".
			preserveGrokOAuthCredentials(&acc, existing)
			preserveGrokRuntimeStateOnAdminEdit(&acc, existing)
			if grokAccountIsOAuth(&acc) && !grokAccountHasOAuthCredentials(&acc) {
				http.Error(w, "missing oauth token", http.StatusBadRequest)
				return
			}
		} else if strings.EqualFold(acc.AccountType, "workbuddy") {
			// The read path redacts the refresh token, so an ordinary edit
			// arrives without it; keep the stored credential unless a new one
			// was actually submitted.
			PreserveWorkBuddyCredentialsOnEdit(&acc, existing)
			if resolveWorkBuddyCredentials(&acc).RefreshToken == "" && resolveWorkBuddyCredentials(&acc).AccessToken == "" {
				http.Error(w, "missing WorkBuddy credential", http.StatusBadRequest)
				return
			}
			NormalizeWorkBuddyCredentials(&acc)
		}

		isWarpAccount := strings.EqualFold(acc.AccountType, "warp")
		if !isWarpAccount && acc.SessionID == "" {
			acc.SessionID = existing.SessionID
		}
		if isWarpAccount {
			if strings.TrimSpace(acc.DeviceID) == "" {
				acc.DeviceID = existing.DeviceID
			}
			if strings.TrimSpace(acc.RequestID) == "" {
				acc.RequestID = existing.RequestID
			}
		}
		// For Grok SSO accounts, empty cookie on edit should keep the existing
		// credential the same way Warp keeps refresh tokens.
		if strings.EqualFold(acc.AccountType, "grok") && !grokAccountIsOAuth(&acc) {
			if strings.TrimSpace(acc.ClientCookie) == "" {
				acc.ClientCookie = existing.ClientCookie
			}
			if strings.TrimSpace(acc.RefreshToken) == "" {
				acc.RefreshToken = existing.RefreshToken
			}
		}
		if !isWarpAccount && acc.SessionCookie == "" {
			acc.SessionCookie = existing.SessionCookie
		}
		if !isWarpAccount && acc.ClientUat == "" {
			acc.ClientUat = existing.ClientUat
		}
		if !isWarpAccount && acc.ProjectID == "" {
			acc.ProjectID = existing.ProjectID
		}
		if acc.UserID == "" {
			acc.UserID = existing.UserID
		}
		if acc.Email == "" {
			acc.Email = existing.Email
		}
		if duplicate, err := a.findDuplicateAccountByCredential(r.Context(), &acc, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if duplicate != nil {
			http.Error(w, duplicateAccountError(duplicate).Error(), http.StatusConflict)
			return
		}

		if err := a.store.UpdateAccount(r.Context(), &acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.syncGrokSSOProviderView(r.Context(), &acc); err != nil {
			slog.Error("Failed to synchronize linked Grok Console SSO account", "account_id", acc.ID, "error", err)
			if isGrokSSOAccount(existing) && existing.GrokSSOParentID == 0 && grok.ProviderForAccount(existing) == grok.ProviderWeb {
				if rollbackErr := a.store.UpdateAccount(r.Context(), existing); rollbackErr != nil {
					slog.Error("Failed to restore Grok Web SSO source after synchronization error", "account_id", existing.ID, "error", rollbackErr)
				}
			}
			http.Error(w, "failed to synchronize linked Grok Console SSO account", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(normalizeAccountOutput(&acc))

	case http.MethodDelete:
		if strings.EqualFold(account.AccountType, "grok") && !grokAccountIsOAuth(account) {
			if err := a.deleteGrokSSOSourceAndLinkedConsoleAccounts(r.Context(), account.ID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := a.store.DeleteAccount(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleGrokAvailability(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	counts := map[string]int{
		grok.ProviderBuild:   0,
		grok.ProviderWeb:     0,
		grok.ProviderConsole: 0,
	}
	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, acc := range accounts {
		if acc == nil || !acc.Enabled {
			continue
		}
		if provider := grok.ProviderForAccount(acc); provider != "" {
			if _, ok := counts[provider]; ok {
				counts[provider]++
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"counts": counts})
}

func (a *API) HandleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exportData := ExportData{
		Version:  1,
		ExportAt: time.Now(),
		Accounts: make([]store.Account, 0, len(accounts)),
	}
	for _, acc := range accounts {
		// Linked accounts intentionally share a credential but represent two
		// provider-specific runtime records. Export the Web source only; import
		// reconciliation rebuilds or links its Console companion.
		if isLinkedGrokConsoleAccount(acc) {
			continue
		}
		// Warp sessions are not portable credentials. Log in again on the target
		// server; exporting them would recreate the removed token-import path.
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
			continue
		}
		normalized := *normalizeAccountOutput(acc).Account
		// Export must preserve OAuth credentials (normalizeAccountOutput hides
		// them for list/query responses); an OAuth export that drops them is
		// unusable on re-import.
		if grokAccountIsOAuth(acc) {
			normalized.OAuthAccessToken = acc.OAuthAccessToken
			normalized.OAuthRefreshToken = acc.OAuthRefreshToken
			normalized.OAuthExpiresAt = acc.OAuthExpiresAt
		}
		normalized.ID = 0
		normalized.RequestCount = 0
		exportData.Accounts = append(exportData.Accounts, normalized)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=accounts_export.json")
	json.NewEncoder(w).Encode(exportData)
}

func (a *API) HandleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var exportData ExportData
	if err := json.NewDecoder(r.Body).Decode(&exportData); err != nil {
		http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	result := ImportResult{Total: len(exportData.Accounts)}

	for _, acc := range exportData.Accounts {
		if grok.IsLinkedConsoleSSOCompanion(&acc) {
			result.Skipped++
			continue
		}
		acc.ID = 0
		acc.RequestCount = 0
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		acc.GrokSSOParentID = 0
		if strings.TrimSpace(acc.AccountType) == "" {
			result.Skipped++
			continue
		}
		if !isSupportedAccountType(acc.AccountType) {
			result.Skipped++
			continue
		}
		if strings.EqualFold(acc.AccountType, "warp") {
			result.Skipped++
			continue
		} else if strings.EqualFold(acc.AccountType, "grok") {
			normalizeGrokTokenInput(&acc)
			if !grokAccountIsOAuth(&acc) {
				// Imports own one visible Web source; reconciliation creates or
				// restores its linked Console account.
				acc.GrokProvider = grok.ProviderWeb
				acc.GrokSSOParentID = 0
			}
			if grokAccountIsOAuth(&acc) && !grokAccountHasOAuthCredentials(&acc) {
				slog.Warn("Skipped grok oauth import without credentials", "name", acc.Name)
				result.Skipped++
				continue
			}
		}
		if strings.EqualFold(acc.AccountType, "grok") && !grokAccountIsOAuth(&acc) {
			if err := a.reconcileGrokSSOProviderCredential(r.Context(), &acc); err != nil {
				slog.Warn("Failed to repair linked Grok Console SSO account before import", "name", acc.Name, "error", err)
				result.Skipped++
				continue
			}
			if existing, err := a.findDuplicateAccountByCredential(r.Context(), &acc, 0); err != nil {
				slog.Warn("Failed to detect duplicate imported Grok SSO account", "name", acc.Name, "error", err)
				result.Skipped++
				continue
			} else if existing != nil {
				result.Skipped++
				continue
			}
		}
		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Warn("Failed to import account", "name", acc.Name, "error", err)
			result.Skipped++
		} else {
			if strings.EqualFold(acc.AccountType, "grok") && !grokAccountIsOAuth(&acc) {
				if err := a.syncGrokSSOProviderView(r.Context(), &acc); err != nil {
					slog.Warn("Failed to link imported Grok Console SSO account", "account_id", acc.ID, "error", err)
					if cleanupErr := a.deleteGrokSSOSourceAndLinkedConsoleAccounts(r.Context(), acc.ID); cleanupErr != nil {
						slog.Error("Failed to compensate incomplete imported Grok SSO pair", "account_id", acc.ID, "error", cleanupErr)
					}
					result.Skipped++
					continue
				}
			}
			result.Imported++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func generateApiKey() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, 48)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return "sk-" + string(b), nil
}

func (a *API) HandleKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		keys, err := a.store.ListApiKeys(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(keys)

	case http.MethodPost:
		var req struct {
			Name          string     `json:"name"`
			AllowedModels []string   `json:"allowed_models"`
			RPMLimit      int        `json:"rpm_limit"`
			MaxConcurrent int        `json:"max_concurrent"`
			ExpiresAt     *time.Time `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}
		if req.RPMLimit < 0 {
			http.Error(w, "rpm_limit must be greater than or equal to zero", http.StatusBadRequest)
			return
		}
		if req.MaxConcurrent < 0 || req.MaxConcurrent > 1024 {
			http.Error(w, "max_concurrent must be between 0 and 1024", http.StatusBadRequest)
			return
		}
		if req.ExpiresAt != nil {
			expiresAt := req.ExpiresAt.UTC()
			if !time.Now().UTC().Before(expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			req.ExpiresAt = &expiresAt
		}

		fullKey, err := generateApiKey()
		if err != nil {
			slog.Error("Failed to generate api key", "error", err)
			http.Error(w, "failed to generate api key", http.StatusInternalServerError)
			return
		}

		hash := sha256.Sum256([]byte(fullKey))
		hashStr := hex.EncodeToString(hash[:])
		key := store.ApiKey{
			Name:          req.Name,
			KeyHash:       hashStr,
			KeyFull:       fullKey,
			KeyPrefix:     "sk-",
			KeySuffix:     fullKey[len(fullKey)-4:],
			Enabled:       true,
			AllowedModels: normalizeAllowedModels(req.AllowedModels),
			RPMLimit:      req.RPMLimit,
			MaxConcurrent: req.MaxConcurrent,
			ExpiresAt:     req.ExpiresAt,
		}
		if err := a.store.CreateApiKey(r.Context(), &key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(CreateKeyResponse{
			ID:            key.ID,
			Key:           fullKey,
			Name:          key.Name,
			KeyPrefix:     key.KeyPrefix,
			KeySuffix:     key.KeySuffix,
			Enabled:       key.Enabled,
			AllowedModels: key.AllowedModels,
			RPMLimit:      key.RPMLimit,
			MaxConcurrent: key.MaxConcurrent,
			ExpiresAt:     key.ExpiresAt,
			CreatedAt:     key.CreatedAt,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleKeyByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	idStr := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var req UpdateKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Enabled == nil && req.AllowedModels == nil && req.RPMLimit == nil && req.MaxConcurrent == nil && len(req.ExpiresAt) == 0 {
			http.Error(w, "at least one policy field is required", http.StatusBadRequest)
			return
		}
		key, err := a.store.GetApiKeyByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if req.Enabled != nil {
			key.Enabled = *req.Enabled
		}
		if req.AllowedModels != nil {
			key.AllowedModels = normalizeAllowedModels(*req.AllowedModels)
		}
		if req.RPMLimit != nil {
			if *req.RPMLimit < 0 {
				http.Error(w, "rpm_limit must be greater than or equal to zero", http.StatusBadRequest)
				return
			}
			key.RPMLimit = *req.RPMLimit
		}
		if req.MaxConcurrent != nil {
			if *req.MaxConcurrent < 0 || *req.MaxConcurrent > 1024 {
				http.Error(w, "max_concurrent must be between 0 and 1024", http.StatusBadRequest)
				return
			}
			key.MaxConcurrent = *req.MaxConcurrent
		}
		if len(req.ExpiresAt) > 0 {
			expiresAt, err := parseOptionalExpiry(req.ExpiresAt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if expiresAt != nil && !time.Now().UTC().Before(*expiresAt) {
				http.Error(w, "expires_at must be in the future", http.StatusBadRequest)
				return
			}
			key.ExpiresAt = expiresAt
		}
		if err := a.store.UpdateApiKey(r.Context(), key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(key)

	case http.MethodDelete:
		if err := a.store.DeleteApiKey(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		models, err := a.store.ListModels(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(models)

	case http.MethodPost:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := a.store.CreateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(m)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	id := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if id == "" {
		http.Error(w, "Model ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNoRows) || err.Error() == "redis: nil" {
				http.Error(w, "Model not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodPut:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.ID = id

		if err := a.store.UpdateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(m)

	case http.MethodDelete:
		if err := a.store.DeleteModel(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *API) SetTokenCache(c tokencache.Cache) {
	a.tokenCache = c
}

func (a *API) HandleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if a.tokenCache == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := a.tokenCache.Clear(r.Context()); err != nil {
		http.Error(w, "Failed to clear cache: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func tokenCacheConfigChanged(before, after *config.Config) bool {
	if before == nil || after == nil {
		return true
	}
	return before.CacheTokenCount != after.CacheTokenCount ||
		before.CacheTTL != after.CacheTTL ||
		before.CacheStrategy != after.CacheStrategy ||
		before.EnableTokenCache != after.EnableTokenCache ||
		before.TokenCacheTTL != after.TokenCacheTTL ||
		before.TokenCacheStrategy != after.TokenCacheStrategy
}

func (a *API) clearTokenCaches(ctx context.Context) {
	if a.tokenCache != nil {
		if err := a.tokenCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear token cache after config update", "error", err)
		}
	}
	if a.promptCache != nil {
		if err := a.promptCache.Clear(ctx); err != nil {
			slog.Warn("failed to clear prompt cache after config update", "error", err)
		}
	}
}

func configPayload(cfg *config.Config) (map[string]interface{}, error) {
	if cfg == nil {
		return map[string]interface{}{}, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if v, ok := payload["admin_pass"]; ok {
		payload["admin_password"] = v
	}
	if rawProxyURL, ok := payload["proxy_url"].(string); !ok || strings.TrimSpace(rawProxyURL) == "" {
		if proxyURL := util.ProxyURLFromConfig(cfg); proxyURL != nil {
			payload["proxy_url"] = proxyURL.String()
		}
	}
	return payload, nil
}

func buildConfigFromPatch(r *http.Request, current *config.Config) (*config.Config, error) {
	base := &config.Config{}
	if current != nil {
		copyCfg := *current
		base = &copyCfg
	}

	baseMap, err := configPayload(base)
	if err != nil {
		return nil, err
	}

	patch := map[string]interface{}{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		return nil, err
	}

	if v, ok := patch["admin_password"]; ok {
		patch["admin_pass"] = v
	}

	for key, value := range patch {
		baseMap[key] = normalizeConfigPatchValue(key, value)
	}
	if _, ok := patch["proxy_url"]; ok {
		baseMap["proxy_http"] = ""
		baseMap["proxy_https"] = ""
		baseMap["proxy_user"] = ""
		baseMap["proxy_pass"] = ""
	}

	raw, err := json.Marshal(baseMap)
	if err != nil {
		return nil, err
	}
	var newCfg config.Config
	if err := json.Unmarshal(raw, &newCfg); err != nil {
		return nil, err
	}
	return &newCfg, nil
}

func normalizeConfigPatchValue(key string, value interface{}) interface{} {
	if value == nil {
		return nil
	}

	switch key {
	case "enable_token_refresh", "enable_usage_refresh", "enable_token_count", "cache_token_count",
		"enable_token_cache", "auto_refresh_token", "kiro_use_builtin_proxy", "warp_use_builtin_proxy",
		"antigravity_use_builtin_proxy", "warp_credit_refund",
		"enable_context_compress", "debug_enabled":
		if b, ok := parseBoolish(value); ok {
			return b
		}
	case "retry_delay", "request_timeout", "refresh_interval", "cache_ttl", "token_cache_ttl",
		"redis_db", "token_refresh_interval", "load_balancer_cache_ttl", "concurrency_limit",
		"concurrency_timeout", "max_retries", "credential_retries":
		if i, ok := parseIntish(value); ok {
			return i
		}
	case "proxy_bypass":
		return normalizeProxyBypassValue(value)
	case "proxy_url":
		return strings.TrimSpace(fmt.Sprint(value))
	}

	return value
}

func parseBoolish(value interface{}) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		switch s {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func parseIntish(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func normalizeProxyBypassValue(value interface{}) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		lines := strings.FieldsFunc(v, func(r rune) bool {
			return r == '\n' || r == ','
		})
		out := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	default:
		return nil
	}
}

func (a *API) syncAccountAfterCreate(acc store.Account) {
	if !acc.Enabled {
		return
	}

	go func(account store.Account) {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer syncCancel()

		accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &account)
		if syncErr != nil {
			slog.Warn("Initial account sync failed", "account_id", account.ID, "type", account.AccountType, "error", syncErr)
			if accountStatus != "" {
				account.StatusCode = accountStatus
				account.StatusMessage = strings.TrimSpace(syncErr.Error())
				account.LastAttempt = time.Now()
			}
		} else {
			applySuccessfulAccountRefreshStatus(&account, accountStatus)
		}

		if updateErr := a.store.UpdateAccount(context.Background(), &account); updateErr != nil {
			slog.Warn("Failed to persist initial account sync", "account_id", account.ID, "type", account.AccountType, "error", updateErr)
		}
	}(acc)
}

func applySuccessfulAccountRefreshStatus(acc *store.Account, status string) {
	if acc == nil {
		return
	}
	status = strings.TrimSpace(status)
	// The credentials answered the upstream, whatever the verdict: stamp it so a
	// scheduler can tell a verified account from one that was never checked. The
	// policy package owns that pairing so every entrance behaves identically.
	if status == "" {
		accountpolicy.Success(time.Now()).Apply(acc)
		return
	}
	verdict := accountpolicy.Verdict{Status: status, At: time.Now()}
	if verdict.Scope = accountpolicy.ScopeForStatus(status); verdict.Scope == accountpolicy.ScopeCredential {
		verdict.NeedsLogin = true
	}
	verdict.Apply(acc)
}

func (a *API) persistConfig(ctx context.Context, current, newCfg *config.Config) error {
	if newCfg == nil {
		return fmt.Errorf("config is nil")
	}
	if a.store == nil {
		return fmt.Errorf("settings store not configured")
	}

	config.ApplyHardcoded(newCfg)

	data, err := json.Marshal(newCfg)
	if err != nil {
		return err
	}

	// Keep the original shared config pointer updated in place so long-lived
	// components started with that pointer (handler/background loops/providers)
	// observe runtime config changes such as proxy updates immediately.
	storedCfg := newCfg
	if current != nil {
		*current = *newCfg
		storedCfg = current
	}
	a.config.Store(storedCfg)
	if err := a.store.SetSetting(ctx, "config", string(data)); err != nil {
		return err
	}
	if tokenCacheConfigChanged(current, newCfg) {
		a.clearTokenCaches(ctx)
	}
	return nil
}
