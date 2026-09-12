package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/modelpolicy"
)

var (
	ErrNoRows            = fmt.Errorf("no rows in result set")
	ErrApiKeyExpired     = fmt.Errorf("api key expired")
	ErrApiKeyRateLimited = fmt.Errorf("api key rate limit exceeded")
)

type Account struct {
	ID                   int64     `json:"id"`
	Name                 string    `json:"name"`
	AccountType          string    `json:"account_type"`
	NSFWEnabled          bool      `json:"nsfw_enabled"`
	SessionID            string    `json:"session_id"`
	ClientCookie         string    `json:"client_cookie"`
	RefreshToken         string    `json:"refresh_token,omitempty"`
	DeviceID             string    `json:"device_id,omitempty"`
	RequestID            string    `json:"request_id,omitempty"`
	SessionCookie        string    `json:"session_cookie"`
	ClientUat            string    `json:"client_uat"`
	ProjectID            string    `json:"project_id"`
	UserID               string    `json:"user_id"`
	AgentMode            string    `json:"agent_mode"`
	Email                string    `json:"email"`
	Weight               int       `json:"weight"`
	MaxConcurrent        int       `json:"max_concurrent,omitempty"`
	Enabled              bool      `json:"enabled"`
	Token                string    `json:"token"`        // Runtime/display token for non-Warp channels
	Subscription         string    `json:"subscription"` // "free", "pro", etc.
	UsageCurrent         float64   `json:"usage_current"`
	UsageTotal           float64   `json:"usage_total"` // Used as lifetime usage
	UsageLimit           float64   `json:"usage_limit"` // Daily limit
	WarpMonthlyLimit     float64   `json:"warp_monthly_limit,omitempty"`
	WarpMonthlyRemaining float64   `json:"warp_monthly_remaining,omitempty"`
	WarpBonusRemaining   float64   `json:"warp_bonus_remaining,omitempty"`
	StatusCode           string    `json:"status_code"`
	LastAttempt          time.Time `json:"last_attempt"`
	QuotaResetAt         time.Time `json:"quota_reset_at"`
	RequestCount         int64     `json:"request_count"`
	LastUsedAt           time.Time `json:"last_used_at"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`

	// CredentialType marks the Grok account credential mode. Empty or "sso"
	// keeps the legacy SSO-cookie behavior; "oauth" selects the Build CLI OAuth
	// flow (cli-chat-proxy.grok.com + Bearer). Zero value must preserve legacy
	// SSO semantics for old Redis records.
	CredentialType    string    `json:"credential_type,omitempty"`
	OAuthAccessToken  string    `json:"oauth_access_token,omitempty"`
	OAuthRefreshToken string    `json:"oauth_refresh_token,omitempty"`
	OAuthExpiresAt    time.Time `json:"oauth_expires_at,omitempty"`
	TeamID            string    `json:"team_id,omitempty"`
	// UpstreamMode overrides the per-account upstream selection. Empty lets the
	// ModelSpec decide; otherwise one of "app_chat", "console", "cli".
	UpstreamMode string `json:"upstream_mode,omitempty"`
	// GrokProvider is the explicit xAI product boundary. Build OAuth, Grok Web
	// SSO and Console SSO have different credentials, model catalogs, quotas
	// and failure semantics; they must not be treated as interchangeable.
	// Legacy accounts are normalized on read/write from CredentialType.
	GrokProvider string `json:"grok_provider,omitempty"`
	// GrokSSOParentID links an internal Console SSO runtime child to its visible
	// Web SSO source. The child inherits credential/source scheduling settings
	// but keeps independent model cache, quota, health and request state.
	GrokSSOParentID int64 `json:"grok_sso_parent_id,omitempty"`
	// GrokModels is the last successful account-specific upstream /v1/models
	// capability snapshot. An empty snapshot means not synced yet, not that the
	// account supports every model.
	GrokModels             []string  `json:"grok_models,omitempty"`
	GrokModelsSyncedAt     time.Time `json:"grok_models_synced_at,omitempty"`
	MissingThinkingStrikes int       `json:"missing_thinking_strikes,omitempty"`
	MissingThinkingLastAt  time.Time `json:"missing_thinking_last_at,omitempty"`
	// GrokBilling contains only official xAI Build billing information. It is
	// deliberately separate from GrokRateLimits, whose request/token headers
	// are short-lived throttling windows rather than subscription allowance.
	GrokBilling    GrokBillingSnapshot   `json:"grok_billing,omitempty"`
	GrokRateLimits GrokRateLimitSnapshot `json:"grok_rate_limits,omitempty"`
	// GrokWebQuota stores the Web SSO quota windows returned by the upstream
	// auto/fast modes. It is intentionally separate from Build billing and
	// passive request/token rate-limit headers.
	GrokWebQuota GrokWebQuotaSnapshot `json:"grok_web_quota,omitempty"`

	// WorkBuddyAccessToken is the short-lived Keycloak bearer token of a
	// WorkBuddy (www.workbuddy.ai) account. WorkBuddyRefreshToken is the
	// durable credential and is ROTATED by Keycloak on every refresh, so the
	// rotated value must be written back. The refresh token is kept in its own
	// field instead of the generic RefreshToken slot so account responses can
	// redact it without touching other channels.
	WorkBuddyAccessToken  string    `json:"workbuddy_access_token,omitempty"`
	WorkBuddyRefreshToken string    `json:"workbuddy_refresh_token,omitempty"`
	WorkBuddyExpiresAt    time.Time `json:"workbuddy_expires_at,omitempty"`
	WorkBuddyUID          string    `json:"workbuddy_uid,omitempty"`
	// WorkBuddyModelIDs is the last successful account-scoped /v3/config `cli`
	// whitelist snapshot. An empty snapshot means "not synced yet", not "the
	// account supports every model".
	WorkBuddyModelIDs       []string  `json:"workbuddy_model_ids,omitempty"`
	WorkBuddyModelsSyncedAt time.Time `json:"workbuddy_models_synced_at,omitempty"`
	// WorkBuddyQuota is the last successful credit-meter snapshot. The generic
	// UsageLimit/UsageCurrent fields stay authoritative for scheduling; this
	// keeps the extra detail the meter reports (cycle reset, package label and
	// the consumption delta the account table shows).
	WorkBuddyQuota WorkBuddyQuotaSnapshot `json:"workbuddy_quota,omitempty"`
}

// WorkBuddyQuotaSnapshot is the WorkBuddy credit-meter snapshot. Remaining/Limit
// describe the current cycle; Used/LastConsumedUnits are whole-credit figures
// derived from the meter, because the upstream also reports fractions.
type WorkBuddyQuotaSnapshot struct {
	Limit             float64   `json:"limit,omitempty"`
	Remaining         float64   `json:"remaining,omitempty"`
	Used              float64   `json:"used,omitempty"`
	PackageRemaining  float64   `json:"package_remaining,omitempty"`
	LastConsumedUnits int       `json:"last_consumed_units,omitempty"`
	ResetAt           time.Time `json:"reset_at,omitempty"`
	PeriodEnd         time.Time `json:"period_end,omitempty"`
	PackageName       string    `json:"package_name,omitempty"`
	Unit              string    `json:"unit,omitempty"`
	SyncedAt          time.Time `json:"synced_at,omitempty"`
}

// ResyncAt reports when the quota snapshot needs refreshing. The cycle reset is
// the hard deadline: the allowance is re-armed then, but the console also wants
// the displayed number to stay current between resets.
func (s WorkBuddyQuotaSnapshot) ResyncAt() time.Time {
	if s.SyncedAt.IsZero() {
		return time.Time{}
	}
	if s.ResetAt.IsZero() {
		return s.SyncedAt
	}
	return s.ResetAt
}

// GrokQuotaWindow is one explicit upstream usage or throttling dimension.
// Values are meaningful only when their Has* marker is true; zero is valid.
type GrokQuotaWindow struct {
	Limit        float64   `json:"limit,omitempty"`
	Remaining    float64   `json:"remaining,omitempty"`
	UsagePercent float64   `json:"usage_percent,omitempty"`
	HasLimit     bool      `json:"has_limit,omitempty"`
	HasRemaining bool      `json:"has_remaining,omitempty"`
	HasUsage     bool      `json:"has_usage,omitempty"`
	ResetAt      time.Time `json:"reset_at,omitempty"`
}

// GrokBillingSnapshot stores official Build weekly/monthly windows only.
type GrokBillingSnapshot struct {
	Weekly   GrokQuotaWindow `json:"weekly,omitempty"`
	Monthly  GrokQuotaWindow `json:"monthly,omitempty"`
	SyncedAt time.Time       `json:"synced_at,omitempty"`
	Source   string          `json:"source,omitempty"`
}

// GrokRateLimitSnapshot stores passive response headers separately from
// billing. They can be useful for cooldown and diagnostics but must never be
// rendered as a paid-plan balance.
type GrokRateLimitSnapshot struct {
	Requests   GrokQuotaWindow `json:"requests,omitempty"`
	Tokens     GrokQuotaWindow `json:"tokens,omitempty"`
	Model      string          `json:"model,omitempty"`
	ObservedAt time.Time       `json:"observed_at,omitempty"`
}

// GrokWebQuotaSnapshot is the authoritative Web SSO quota snapshot. Either
// mode may be unavailable for a given account, so each window carries its own
// presence markers and the snapshot can represent a partial response.
type GrokWebQuotaSnapshot struct {
	Auto     GrokQuotaWindow `json:"auto,omitempty"`
	Fast     GrokQuotaWindow `json:"fast,omitempty"`
	SyncedAt time.Time       `json:"synced_at,omitempty"`
	Source   string          `json:"source,omitempty"`
}

// AccountStatusWarpQuotaExhausted records a Warp credit exhaustion separately
// from a transient HTTP 429. The account remains usable for Warp's free-only
// capabilities while model/capability filters keep paid requests away from it.
const AccountStatusWarpQuotaExhausted = "warp_quota_exhausted"

type ApiKey struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	KeyHash       string     `json:"-"`
	KeyFull       string     `json:"-"`
	KeyPrefix     string     `json:"key_prefix"`
	KeySuffix     string     `json:"key_suffix"`
	Enabled       bool       `json:"enabled"`
	AllowedModels []string   `json:"allowed_models,omitempty"`
	RPMLimit      int        `json:"rpm_limit,omitempty"`
	MaxConcurrent int        `json:"max_concurrent,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

// StoredResponse records the ownership needed to continue or manage an
// upstream Responses resource without retaining the request or response body.
type StoredResponse struct {
	ResponseID     string    `json:"response_id"`
	OwnerHash      string    `json:"owner_hash"`
	AccountID      int64     `json:"account_id"`
	Model          string    `json:"model"`
	Provider       string    `json:"provider"`
	PromptCacheKey string    `json:"prompt_cache_key,omitempty"`
	ContentType    string    `json:"content_type,omitempty"`
	Body           []byte    `json:"body,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// StoredReasoningReplay contains one opaque encrypted reasoning item. The key
// is already tenant/model/session isolated by the gateway; Redis persistence
// lets later turns resume on another replica without storing plaintext chain
// of thought.
type StoredReasoningReplay struct {
	Model      string `json:"model"`
	SessionKey string `json:"session_key"`
	// EncryptedContent is the legacy single-cipher form. It is still written by
	// paths that only observe one opaque reasoning item, and is always read for
	// compatibility; Items takes precedence when present.
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Items            []json.RawMessage `json:"items,omitempty"`
	ExpiresAt        time.Time         `json:"expires_at"`
}

// StoredPuterReasoningReplay keeps the reasoning associated with an emitted
// Puter tool call. Some Anthropic-compatible clients display thinking blocks
// but replace them with "..." in the next request, so the tool call ID is the
// stable continuation key. Implementations must encrypt ReasoningContent at
// rest before writing it to the backing store.
type StoredPuterReasoningReplay struct {
	Model            string    `json:"model"`
	ToolCallID       string    `json:"tool_call_id"`
	ReasoningContent string    `json:"reasoning_content"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type StoredSessionAffinity struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	SessionKey string    `json:"session_key"`
	AccountID  int64     `json:"account_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// StoredVideoJob is the durable, owner-scoped state required to retrieve an
// asynchronous video result after the serving process restarts. Media bytes
// remain in the configured local cache; this record only stores metadata.
type StoredVideoJob struct {
	ID                string    `json:"id"`
	OwnerHash         string    `json:"owner_hash"`
	AccountID         int64     `json:"account_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Model             string    `json:"model"`
	Prompt            string    `json:"prompt,omitempty"`
	Seconds           int       `json:"seconds,omitempty"`
	Size              string    `json:"size,omitempty"`
	Quality           string    `json:"quality,omitempty"`
	Status            string    `json:"status"`
	Progress          int       `json:"progress"`
	VideoURL          string    `json:"video_url,omitempty"`
	ContentPath       string    `json:"content_path,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
	BuildFallback     bool      `json:"build_fallback,omitempty"`
	RemixedFromID     string    `json:"remixed_from_id,omitempty"`
	Operation         string    `json:"operation,omitempty"`
	StandardAPI       bool      `json:"standard_api,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         int64     `json:"created_at"`
	CompletedAt       int64     `json:"completed_at,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// StoredMediaInput points at an immutable temporary image or video accepted
// for later use through a standard API file_id.
type StoredMediaInput struct {
	ID          string    `json:"id"`
	OwnerHash   string    `json:"owner_hash"`
	Kind        string    `json:"kind"`
	MIMEType    string    `json:"mime_type"`
	ContentPath string    `json:"content_path"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type Store struct {
	accounts  accountStore
	settings  settingsStore
	apiKeys   apiKeyStore
	models    modelStore
	responses responseStore
	reasoning reasoningReplayStore
	videoJobs videoJobStore
	media     mediaInputStore
}

type Options struct {
	StoreMode               string
	RedisAddr               string
	RedisPassword           string
	RedisDB                 int
	RedisPrefix             string
	CredentialEncryptionKey []byte
}

type accountStore interface {
	CreateAccount(ctx context.Context, acc *Account) error
	UpdateAccount(ctx context.Context, acc *Account) error
	DeleteAccount(ctx context.Context, id int64) error
	GetAccount(ctx context.Context, id int64) (*Account, error)
	ListAccounts(ctx context.Context) ([]*Account, error)
	GetEnabledAccounts(ctx context.Context) ([]*Account, error)
	IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error
}

type settingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

type apiKeyStore interface {
	CreateApiKey(ctx context.Context, key *ApiKey) error
	ListApiKeys(ctx context.Context) ([]*ApiKey, error)
	UpdateApiKey(ctx context.Context, key *ApiKey) error
	DeleteApiKey(ctx context.Context, id int64) error
	GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error)
	GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error)
	ConsumeApiKeyRPM(ctx context.Context, id int64, limit int, now time.Time) (bool, error)
}

type modelStore interface {
	CreateModel(ctx context.Context, m *Model) error
	UpdateModel(ctx context.Context, m *Model) error
	DeleteModel(ctx context.Context, id string) error
	GetModel(ctx context.Context, id string) (*Model, error)
	ListModels(ctx context.Context) ([]*Model, error)
	GetModelByModelID(ctx context.Context, modelID string) (*Model, error)
	GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error)
}

type responseStore interface {
	SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error
	GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error)
	DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error
}

type reasoningReplayStore interface {
	DeleteReasoningReplay(ctx context.Context, model, sessionKey string) error
	SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error
	GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error)
	SavePuterReasoningReplay(ctx context.Context, replay *StoredPuterReasoningReplay, ttl time.Duration) error
	GetPuterReasoningReplay(ctx context.Context, model, toolCallID string) (*StoredPuterReasoningReplay, error)
	SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error
	GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error)
}

type videoJobStore interface {
	SaveStoredVideoJob(ctx context.Context, job *StoredVideoJob, ttl time.Duration) error
	GetStoredVideoJob(ctx context.Context, id, ownerHash string) (*StoredVideoJob, error)
	ListStoredVideoJobs(ctx context.Context) ([]*StoredVideoJob, error)
	AcquireVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error)
	RefreshVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error)
	ReleaseVideoJobLease(ctx context.Context, id, ownerHash, holder string) (bool, error)
}

type mediaInputStore interface {
	SaveStoredMediaInput(ctx context.Context, input *StoredMediaInput, ttl time.Duration) error
	GetStoredMediaInput(ctx context.Context, id, ownerHash string) (*StoredMediaInput, error)
	DeleteStoredMediaInput(ctx context.Context, id, ownerHash string) error
}

func New(opts Options) (*Store, error) {
	store := &Store{}
	redisStore, err := newRedisStore(opts.RedisAddr, opts.RedisPassword, opts.RedisDB, opts.RedisPrefix, opts.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to init redis store: %w", err)
	}
	store.accounts = redisStore
	store.settings = redisStore
	store.apiKeys = redisStore
	store.models = redisStore
	store.responses = redisStore
	store.reasoning = redisStore
	store.videoJobs = redisStore
	store.media = redisStore
	if err := redisStore.migrateLegacyAccountCredentials(context.Background()); err != nil {
		_ = redisStore.Close()
		return nil, fmt.Errorf("failed to migrate account credentials: %w", err)
	}
	store.seedModels()
	return store, nil
}

func (s *Store) seedModels() {
	ctx := context.Background()
	s.cleanupDeprecatedModelIDs(ctx)
	s.reconcileLatestPuterModels(ctx)
	s.reconcileLatestWorkBuddyModels(ctx)
	existing, err := s.ListModels(ctx)
	if err == nil && len(existing) > 0 {
		s.ensureRequiredGrokChatModels(ctx)
		s.backfillGrokRouteMetadata(ctx)
		slog.Debug("Model seed skipped; existing model records preserved", "count", len(existing))
		return
	}
	if err != nil {
		slog.Warn("failed to inspect existing models before seed", "error", err)
	}

	models := BuildWarpSeedModels()
	models = append(models, buildGrokSeedModels()...)
	models = append(models, buildPuterSeedModels()...)
	models = append(models, buildWorkBuddySeedModels()...)

	for _, m := range models {
		if _, err := s.GetModelByChannelAndModelID(ctx, m.Channel, m.ModelID); err == nil {
			continue
		}
		if err := s.CreateModel(ctx, &m); err != nil {
			slog.Warn("Failed to seed model", "model_id", m.ModelID, "error", err)
		} else {
			slog.Debug("Seeded model", "model_id", m.ModelID)
		}
	}

	s.cleanupDeprecatedModelIDs(ctx)
	s.reconcileLatestPuterModels(ctx)
	s.reconcileLatestWorkBuddyModels(ctx)

}

func (s *Store) backfillGrokRouteMetadata(ctx context.Context) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "grok") {
			continue
		}
		// Older catalog defaults used the public video ID as the Web model name.
		// Repair only that exact mapping, preserving custom/provider routes.
		if model.ModelID == "grok-imagine-video" && model.Provider == "web" && model.UpstreamModel == "grok-imagine-video" {
			updated := *model
			updated.UpstreamModel = "imagine-video-gen"
			if err := s.UpdateModel(ctx, &updated); err != nil {
				slog.Warn("failed to repair Grok video route", "model_id", model.ModelID, "error", err)
			}
			continue
		}
		if model.Provider != "" && model.UpstreamModel != "" && len(model.Capabilities) > 0 {
			continue
		}
		updated := *model
		applyGrokRouteDefaults(&updated)
		if err := s.UpdateModel(ctx, &updated); err != nil {
			slog.Warn("failed to backfill Grok route metadata", "model_id", model.ModelID, "error", err)
		}
	}
}

func (s *Store) reconcileLatestPuterModels(ctx context.Context) {
	models, err := s.ListModels(ctx)
	if err != nil {
		slog.Warn("Failed to inspect Puter models for reconciliation", "error", err)
		return
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "puter") || modelpolicy.IsLatestPuterModelID(model.ModelID) {
			continue
		}
		if err := s.DeleteModel(ctx, model.ID); err != nil {
			slog.Warn("Failed to remove old Puter model", "model_id", model.ModelID, "error", err)
		}
	}
}

func (s *Store) cleanupDeprecatedModelIDs(ctx context.Context) {
	deprecatedModelIDs := []string{
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-0309",
		"grok-4.20-0309-reasoning",
		"grok-4.20-0309-non-reasoning-super",
		"grok-4.20-0309-super",
		"grok-4.20-0309-reasoning-super",
		"grok-4.20-0309-non-reasoning-heavy",
		"grok-4.20-0309-heavy",
		"grok-4.20-0309-reasoning-heavy",
		"grok-4.20-multi-agent-0309",
		"grok-4.20-fast",
		"grok-4.20-auto",
		"grok-4.20-expert",
		"grok-4.20-heavy",
		"grok-4.3-beta",
		"grok-imagine-image-pro",
		"grok-3",
		"grok-3-thinking",
		"grok-3-fast",
		"grok-4",
		"grok-4-mini",
		"grok-4-fast",
		"grok-4-heavy",
		"grok-4.1-mini",
		"grok-4.1-fast",
		"grok-4.1-thinking",
		"grok-4.1",
		"grok-4-1-thinking-1129",
		"grok-4.2",
		"grok-4.20-beta",
		"grok-4.20-reasoning",
		"grok-4.20-non-reasoning",
		"grok-4.20-multi-agent",
		"grok-420",
		"grok-4.3",
		"grok-build-0.1",
		"grok-code-fast",
		"grok-code-fast-1",
		"grok-imagine-1.0",
		"grok-imagine-1.0-fast",
		"grok-imagine-1.0-edit",
		"grok-imagine-1.0-video",
		"grok-2",
		"grok-2.1",
		"grok-3.1",
		"grok-4.21",
	}
	for _, modelID := range deprecatedModelIDs {
		m, err := s.GetModelByModelID(ctx, modelID)
		if err != nil || m == nil {
			continue
		}
		if err := s.DeleteModel(ctx, m.ID); err != nil {
			slog.Warn("Failed to remove deprecated model", "model_id", modelID, "error", err)
			continue
		}
		slog.Debug("Removed deprecated model", "model_id", modelID)
	}
}

func (s *Store) ensureRequiredGrokChatModels(ctx context.Context) {
	for _, src := range buildGrokSeedModels() {
		if _, err := s.GetModelByChannelAndModelID(ctx, src.Channel, src.ModelID); err == nil {
			continue
		}
		record := src
		record.ID = ""
		if err := s.CreateModel(ctx, &record); err != nil {
			slog.Warn("Failed to ensure Grok app-chat model", "model_id", src.ModelID, "error", err)
			continue
		}
		slog.Debug("Ensured Grok app-chat model", "model_id", src.ModelID)
	}
}

func buildGrokSeedModels() []Model {
	items := []struct {
		id   string
		name string
	}{
		{"grok-composer-2.5-fast", "Grok Composer 2.5 Fast"},
		{"grok-4.6", "Grok 4.6"},
		{"grok-4.5", "Grok 4.5"},
		{"console/grok-imagine-image", "Console Grok Imagine Image"},
		{"console/grok-imagine-image-quality", "Console Grok Imagine Image Quality"},
		{"console/grok-imagine-image-2.0", "Console Grok Imagine Image 2.0"},
		{"grok-imagine-image-lite", "Grok Imagine Image Lite"},
		{"grok-imagine-image", "Grok Imagine Image"},
		{"grok-imagine-image-2.0", "Grok Imagine Image 2.0"},
		{"grok-imagine-image-quality", "Grok Imagine Image Quality"},
		{"grok-imagine-image-edit", "Grok Imagine Image Edit"},
		{"grok-imagine-video", "Grok Imagine Video"},
		{"grok-imagine-video-1.5", "Grok Imagine Video 1.5"},
		{"build/grok-imagine-video-1.5", "Build Grok Imagine Video 1.5"},
		{"grok-voice-latest", "Grok Voice Latest"},
		{"grok-voice-think-fast-2.0", "Grok Voice Think Fast 2.0"},
		{"grok-voice-think-fast-1.0", "Grok Voice Think Fast 1.0"},
		{"grok-stt", "Grok Speech to Text"},
	}
	models := make([]Model, 0, len(items))
	for i, item := range items {
		model := Model{
			ID:        fmt.Sprintf("grok-%03d", i+1),
			Channel:   "Grok",
			ModelID:   item.id,
			Name:      item.name,
			Status:    ModelStatusAvailable,
			Verified:  true,
			IsDefault: i == 0,
			SortOrder: i,
		}
		applyGrokRouteDefaults(&model)
		models = append(models, model)
	}
	return models
}

func applyGrokRouteDefaults(model *Model) {
	if model == nil {
		return
	}
	id := strings.ToLower(strings.TrimSpace(model.ModelID))
	model.Origin = "catalog"
	model.UpstreamModel = strings.TrimPrefix(strings.TrimPrefix(id, "console/"), "build/")
	if id == "grok-imagine-video" {
		model.UpstreamModel = "imagine-video-gen"
	}
	switch {
	case strings.HasPrefix(id, "console/"), strings.HasPrefix(id, "grok-voice"), id == "grok-stt", id == "grok-imagine-video-1.5":
		model.Provider = "console"
	case strings.HasPrefix(id, "build/"), id == "grok-composer-2.5-fast", id == "grok-4.5", id == "grok-4.6":
		model.Provider = "build"
	default:
		model.Provider = "web"
	}
	switch {
	case strings.Contains(id, "voice"):
		model.Capabilities = []string{CapabilityRealtime, CapabilityTTS}
	case id == "grok-stt":
		model.Capabilities = []string{CapabilitySTT}
	case strings.Contains(id, "video"):
		model.Capabilities = []string{CapabilityVideo}
		if model.Provider == "web" {
			model.Capabilities = append(model.Capabilities, CapabilityChat)
		}
	case strings.Contains(id, "image-edit"):
		model.Capabilities = []string{CapabilityImageEdit}
		if model.Provider == "web" {
			model.Capabilities = append(model.Capabilities, CapabilityChat)
		}
	case strings.Contains(id, "image"):
		model.Capabilities = []string{CapabilityImage, CapabilityImageEdit}
		if model.Provider == "web" {
			model.Capabilities = append(model.Capabilities, CapabilityChat)
		}
	default:
		model.Capabilities = []string{CapabilityChat, CapabilityMessages, CapabilityResponses}
	}
	model.NormalizeRoute()
}

// ApplyGrokRouteDefaults initializes route metadata for catalog/discovery
// records while allowing callers to override Origin afterwards.
func ApplyGrokRouteDefaults(model *Model) { applyGrokRouteDefaults(model) }

func (s *Store) Close() error {
	if rs, ok := s.accounts.(*redisStore); ok {
		return rs.Close()
	}
	return nil
}

// RedisClient returns the underlying Redis client, or nil if not using Redis.
func (s *Store) RedisClient() *redis.Client {
	if rs, ok := s.accounts.(*redisStore); ok {
		return rs.Client()
	}
	return nil
}

// RedisPrefix returns the configured key prefix.
func (s *Store) RedisPrefix() string {
	if s.accounts != nil {
		if rs, ok := s.accounts.(*redisStore); ok {
			return rs.prefix
		}
	}
	return "orchids:"
}

func (s *Store) CreateAccount(ctx context.Context, acc *Account) error {
	if s.accounts != nil {
		return s.accounts.CreateAccount(ctx, acc)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) UpdateAccount(ctx context.Context, acc *Account) error {
	if s.accounts != nil {
		return s.accounts.UpdateAccount(ctx, acc)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	if s.accounts != nil {
		return s.accounts.DeleteAccount(ctx, id)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) GetAccount(ctx context.Context, id int64) (*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetAccount(ctx, id)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.ListAccounts(ctx)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) GetEnabledAccounts(ctx context.Context) ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetEnabledAccounts(ctx)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error {
	if s.accounts != nil {
		return s.accounts.IncrementAccountStats(ctx, id, usage, count)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	if s.settings != nil {
		return s.settings.GetSetting(ctx, key)
	}
	return "", fmt.Errorf("settings store not configured")
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if s.settings != nil {
		return s.settings.SetSetting(ctx, key, value)
	}
	return fmt.Errorf("settings store not configured")
}

func (s *Store) CreateApiKey(ctx context.Context, key *ApiKey) error {
	if s.apiKeys != nil {
		return s.apiKeys.CreateApiKey(ctx, key)
	}
	return fmt.Errorf("api keys store not configured")
}

// AuthorizeApiKey authenticates a raw client key and atomically applies its
// optional per-minute request limit. Raw keys are never persisted by this path.
func (s *Store) AuthorizeApiKey(ctx context.Context, raw string) (*ApiKey, error) {
	if s == nil || s.apiKeys == nil {
		return nil, fmt.Errorf("api key store not configured")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoRows
	}
	digest := sha256.Sum256([]byte(raw))
	key, err := s.apiKeys.GetApiKeyByHash(ctx, hex.EncodeToString(digest[:]))
	if err != nil {
		return nil, err
	}
	if key == nil || !key.Enabled {
		return nil, ErrNoRows
	}
	now := time.Now().UTC()
	if key.ExpiresAt != nil && !now.Before(key.ExpiresAt.UTC()) {
		return nil, ErrApiKeyExpired
	}
	allowed, err := s.apiKeys.ConsumeApiKeyRPM(ctx, key.ID, key.RPMLimit, now)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, ErrApiKeyRateLimited
	}
	key.LastUsedAt = &now
	return key, nil
}

func (s *Store) ListApiKeys(ctx context.Context) ([]*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.ListApiKeys(ctx)
	}
	return nil, fmt.Errorf("api keys store not configured")
}

func (s *Store) UpdateApiKey(ctx context.Context, key *ApiKey) error {
	if s != nil && s.apiKeys != nil {
		return s.apiKeys.UpdateApiKey(ctx, key)
	}
	return fmt.Errorf("api keys store not configured")
}

func (s *Store) DeleteApiKey(ctx context.Context, id int64) error {
	if s.apiKeys != nil {
		return s.apiKeys.DeleteApiKey(ctx, id)
	}
	return fmt.Errorf("api keys store not configured")
}

func (s *Store) GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.GetApiKeyByID(ctx, id)
	}
	return nil, fmt.Errorf("api keys store not configured")
}

func (s *Store) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.SaveStoredResponse(ctx, response, ttl)
}

func (s *Store) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.responses == nil {
		return nil, fmt.Errorf("response store not configured")
	}
	return s.responses.GetStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.DeleteStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	if s == nil || s.reasoning == nil {
		return nil
	}
	return s.reasoning.DeleteReasoningReplay(ctx, model, key)
}

func (s *Store) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.SaveReasoningReplay(ctx, replay, ttl)
}

func (s *Store) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.GetReasoningReplay(ctx, model, sessionKey)
}

func (s *Store) SavePuterReasoningReplay(ctx context.Context, replay *StoredPuterReasoningReplay, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.SavePuterReasoningReplay(ctx, replay, ttl)
}

func (s *Store) GetPuterReasoningReplay(ctx context.Context, model, toolCallID string) (*StoredPuterReasoningReplay, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.GetPuterReasoningReplay(ctx, model, toolCallID)
}

func (s *Store) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.SaveSessionAffinity(ctx, affinity, ttl)
}

func (s *Store) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.GetSessionAffinity(ctx, provider, model, sessionKey)
}

func (s *Store) SaveStoredVideoJob(ctx context.Context, job *StoredVideoJob, ttl time.Duration) error {
	if s == nil || s.videoJobs == nil {
		return fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.SaveStoredVideoJob(ctx, job, ttl)
}

func (s *Store) GetStoredVideoJob(ctx context.Context, id, ownerHash string) (*StoredVideoJob, error) {
	if s == nil || s.videoJobs == nil {
		return nil, fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.GetStoredVideoJob(ctx, id, ownerHash)
}

func (s *Store) ListStoredVideoJobs(ctx context.Context) ([]*StoredVideoJob, error) {
	if s == nil || s.videoJobs == nil {
		return nil, fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.ListStoredVideoJobs(ctx)
}

func (s *Store) AcquireVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.videoJobs == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.AcquireVideoJobLease(ctx, id, ownerHash, holder, ttl)
}

func (s *Store) RefreshVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.videoJobs == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.RefreshVideoJobLease(ctx, id, ownerHash, holder, ttl)
}

func (s *Store) ReleaseVideoJobLease(ctx context.Context, id, ownerHash, holder string) (bool, error) {
	if s == nil || s.videoJobs == nil {
		return false, fmt.Errorf("video job store not configured")
	}
	return s.videoJobs.ReleaseVideoJobLease(ctx, id, ownerHash, holder)
}

func (s *Store) SaveStoredMediaInput(ctx context.Context, input *StoredMediaInput, ttl time.Duration) error {
	if s == nil || s.media == nil {
		return fmt.Errorf("media input store not configured")
	}
	return s.media.SaveStoredMediaInput(ctx, input, ttl)
}

func (s *Store) GetStoredMediaInput(ctx context.Context, id, ownerHash string) (*StoredMediaInput, error) {
	if s == nil || s.media == nil {
		return nil, fmt.Errorf("media input store not configured")
	}
	return s.media.GetStoredMediaInput(ctx, id, ownerHash)
}

func (s *Store) DeleteStoredMediaInput(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.media == nil {
		return fmt.Errorf("media input store not configured")
	}
	return s.media.DeleteStoredMediaInput(ctx, id, ownerHash)
}

// Model wrappers

func (s *Store) CreateModel(ctx context.Context, m *Model) error {
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	s.clearOtherModelDefaults(ctx, m, false)
	return s.models.CreateModel(ctx, m)
}

func (s *Store) UpdateModel(ctx context.Context, m *Model) error {
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	s.clearOtherModelDefaults(ctx, m, true)
	return s.models.UpdateModel(ctx, m)
}

func (s *Store) clearOtherModelDefaults(ctx context.Context, m *Model, excludeSelf bool) {
	if !m.IsDefault {
		return
	}
	models, err := s.models.ListModels(ctx)
	if err != nil {
		return
	}
	for _, other := range models {
		if other.Channel != m.Channel || excludeSelf && other.ID == m.ID || !other.IsDefault {
			continue
		}
		other.IsDefault = false
		if err := s.models.UpdateModel(ctx, other); err != nil {
			slog.Warn("Failed to clear default flag on model", "model_id", other.ModelID, "error", err)
		}
	}
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	if s.models != nil {
		return s.models.DeleteModel(ctx, id)
	}
	return fmt.Errorf("models store not configured")
}

func (s *Store) GetModel(ctx context.Context, id string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModel(ctx, id)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByModelID(ctx context.Context, modelID string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModelByModelID(ctx, modelID)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModelByChannelAndModelID(ctx, channel, modelID)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) ListModels(ctx context.Context) ([]*Model, error) {
	if s.models != nil {
		return s.models.ListModels(ctx)
	}
	return nil, fmt.Errorf("models store not configured")
}
