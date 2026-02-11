package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

var ErrNoRows = fmt.Errorf("no rows in result set")

type Account struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	AccountType   string    `json:"account_type"`
	SessionID     string    `json:"session_id"`
	ClientCookie  string    `json:"client_cookie"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	SessionCookie string    `json:"session_cookie"`
	ClientUat     string    `json:"client_uat"`
	ProjectID     string    `json:"project_id"`
	UserID        string    `json:"user_id"`
	AgentMode     string    `json:"agent_mode"`
	Email         string    `json:"email"`
	Weight        int       `json:"weight"`
	Enabled       bool      `json:"enabled"`
	Token         string    `json:"token"`        // Truncated display token
	Subscription  string    `json:"subscription"` // "free", "pro", etc.
	UsageCurrent  float64   `json:"usage_current"`
	UsageTotal    float64   `json:"usage_total"` // Used as lifetime usage
	UsageDaily    float64   `json:"usage_daily"` // Usage for current day
	UsageLimit    float64   `json:"usage_limit"` // Daily limit
	ResetDate     string    `json:"reset_date"`  // YYYY-MM-DD for daily reset
	StatusCode    string    `json:"status_code"`
	LastAttempt   time.Time `json:"last_attempt"`
	QuotaResetAt  time.Time `json:"quota_reset_at"`
	RequestCount  int64     `json:"request_count"`
	LastUsedAt    time.Time `json:"last_used_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Settings struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ApiKey struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"`
	KeyFull    string     `json:"key_full,omitempty"`
	KeyPrefix  string     `json:"key_prefix"`
	KeySuffix  string     `json:"key_suffix"`
	Enabled    bool       `json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Store struct {
	accounts accountStore
	settings settingsStore
	apiKeys  apiKeyStore
	models   modelStore
}

type Options struct {
	StoreMode     string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisPrefix   string
}

type accountStore interface {
	CreateAccount(ctx context.Context, acc *Account) error
	UpdateAccount(ctx context.Context, acc *Account) error
	DeleteAccount(ctx context.Context, id int64) error
	GetAccount(ctx context.Context, id int64) (*Account, error)
	ListAccounts(ctx context.Context) ([]*Account, error)
	GetEnabledAccounts(ctx context.Context) ([]*Account, error)
	IncrementRequestCount(ctx context.Context, id int64) error
	IncrementUsage(ctx context.Context, id int64, usage float64) error
	IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error
}

type settingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

type apiKeyStore interface {
	CreateApiKey(ctx context.Context, key *ApiKey) error
	ListApiKeys(ctx context.Context) ([]*ApiKey, error)
	GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error)
	UpdateApiKeyEnabled(ctx context.Context, id int64, enabled bool) error
	UpdateApiKeyLastUsed(ctx context.Context, id int64) error
	DeleteApiKey(ctx context.Context, id int64) error
	GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error)
}

type modelStore interface {
	CreateModel(ctx context.Context, m *Model) error
	UpdateModel(ctx context.Context, m *Model) error
	DeleteModel(ctx context.Context, id string) error
	GetModel(ctx context.Context, id string) (*Model, error)
	ListModels(ctx context.Context) ([]*Model, error)
}

type closeableStore interface {
	Close() error
}

func New(opts Options) (*Store, error) {
	store := &Store{}
	redisStore, err := newRedisStore(opts.RedisAddr, opts.RedisPassword, opts.RedisDB, opts.RedisPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to init redis store: %w", err)
	}
	store.accounts = redisStore
	store.settings = redisStore
	store.apiKeys = redisStore
	store.models = redisStore
	if err := store.seedModels(); err != nil {
		slog.Warn("failed to seed models in redis", "error", err)
	}
	return store, nil
}

func (s *Store) seedModels() error {
	ctx := context.Background()

	models := []Model{
		// Orchids 模型
		{ID: "6", Channel: "Orchids", ModelID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Status: ModelStatusAvailable, IsDefault: true, SortOrder: 0},
		{ID: "7", Channel: "Orchids", ModelID: "claude-opus-4-5", Name: "Claude Opus 4.5", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 1},
		{ID: "42", Channel: "Orchids", ModelID: "claude-sonnet-4-5-thinking", Name: "Claude Sonnet 4.5 Thinking", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 2},
		{ID: "43", Channel: "Orchids", ModelID: "claude-opus-4-5-thinking", Name: "Claude Opus 4.5 Thinking", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 3},
		{ID: "8", Channel: "Orchids", ModelID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 4},
		{ID: "9", Channel: "Orchids", ModelID: "claude-sonnet-4-20250514", Name: "Claude Sonnet 4", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 5},
		{ID: "10", Channel: "Orchids", ModelID: "claude-3-7-sonnet-20250219", Name: "Claude 3.7 Sonnet", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 6},
		// Warp 模型
		{ID: "60", Channel: "Warp", ModelID: "auto", Name: "Warp Auto", Status: ModelStatusAvailable, IsDefault: true, SortOrder: 0},
		{ID: "61", Channel: "Warp", ModelID: "auto-efficient", Name: "Warp Auto Efficient", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 1},
		{ID: "62", Channel: "Warp", ModelID: "auto-genius", Name: "Warp Auto Genius", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 2},
		{ID: "63", Channel: "Warp", ModelID: "claude-4-5-sonnet", Name: "Claude 4.5 Sonnet (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 3},
		{ID: "71", Channel: "Warp", ModelID: "claude-4-5-sonnet-thinking", Name: "Claude 4.5 Sonnet Thinking (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 4},
		{ID: "64", Channel: "Warp", ModelID: "claude-4-5-opus", Name: "Claude 4.5 Opus (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 5},
		{ID: "72", Channel: "Warp", ModelID: "claude-4-5-opus-thinking", Name: "Claude 4.5 Opus Thinking (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 6},
		{ID: "73", Channel: "Warp", ModelID: "claude-4-6-opus-high", Name: "Claude 4.6 Opus High (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 7},
		{ID: "74", Channel: "Warp", ModelID: "claude-4-6-opus-max", Name: "Claude 4.6 Opus Max (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 8},
		{ID: "75", Channel: "Warp", ModelID: "claude-4-5-haiku", Name: "Claude 4.5 Haiku (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 9},
		{ID: "76", Channel: "Warp", ModelID: "gemini-2-5-pro", Name: "Gemini 2.5 Pro (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 10},
		{ID: "77", Channel: "Warp", ModelID: "gemini-3-pro", Name: "Gemini 3 Pro (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 11},
		{ID: "65", Channel: "Warp", ModelID: "gpt-5-low", Name: "GPT-5 Low (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 12},
		{ID: "78", Channel: "Warp", ModelID: "gpt-5-medium", Name: "GPT-5 Medium (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 13},
		{ID: "79", Channel: "Warp", ModelID: "gpt-5-high", Name: "GPT-5 High (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 14},
		{ID: "80", Channel: "Warp", ModelID: "gpt-5-1-low", Name: "GPT-5.1 Low (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 15},
		{ID: "81", Channel: "Warp", ModelID: "gpt-5-1-medium", Name: "GPT-5.1 Medium (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 16},
		{ID: "82", Channel: "Warp", ModelID: "gpt-5-1-high", Name: "GPT-5.1 High (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 17},
		{ID: "83", Channel: "Warp", ModelID: "gpt-5-1-codex-low", Name: "GPT-5.1 Codex Low (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 18},
		{ID: "84", Channel: "Warp", ModelID: "gpt-5-1-codex-medium", Name: "GPT-5.1 Codex Medium (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 19},
		{ID: "85", Channel: "Warp", ModelID: "gpt-5-1-codex-high", Name: "GPT-5.1 Codex High (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 20},
		{ID: "86", Channel: "Warp", ModelID: "gpt-5-1-codex-max-low", Name: "GPT-5.1 Codex Max Low (Warp)", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 21},
		{ID: "70", Channel: "Warp", ModelID: "warp-basic", Name: "Warp Basic", Status: ModelStatusAvailable, IsDefault: false, SortOrder: 22},
	}

	for _, m := range models {
		_, err := s.GetModelByModelID(ctx, m.ModelID)
		if err != nil {
			// Model doesn't exist, create it
			if err := s.CreateModel(ctx, &m); err != nil {
				slog.Warn("Failed to seed model", "model_id", m.ModelID, "error", err)
			} else {
				slog.Info("Seeded model", "model_id", m.ModelID)
			}
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s.accounts != nil {
		if closer, ok := s.accounts.(closeableStore); ok {
			return closer.Close()
		}
	}
	return nil
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

func (s *Store) IncrementRequestCount(ctx context.Context, id int64) error {
	if s.accounts != nil {
		return s.accounts.IncrementRequestCount(ctx, id)
	}
	return fmt.Errorf("store not configured")
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

func (s *Store) ListApiKeys(ctx context.Context) ([]*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.ListApiKeys(ctx)
	}
	return nil, fmt.Errorf("api keys store not configured")
}

func (s *Store) UpdateApiKeyEnabled(ctx context.Context, id int64, enabled bool) error {
	if s.apiKeys != nil {
		return s.apiKeys.UpdateApiKeyEnabled(ctx, id, enabled)
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

// Model wrappers

func (s *Store) CreateModel(ctx context.Context, m *Model) error {
	if s.models != nil {
		if m.IsDefault {
			models, err := s.models.ListModels(ctx)
			if err == nil {
				for _, other := range models {
					if other.Channel == m.Channel && other.IsDefault {
						other.IsDefault = false
						s.models.UpdateModel(ctx, other)
					}
				}
			}
		}
		return s.models.CreateModel(ctx, m)
	}
	return fmt.Errorf("models store not configured")
}

func (s *Store) UpdateModel(ctx context.Context, m *Model) error {
	if s.models != nil {
		if m.IsDefault {
			models, err := s.models.ListModels(ctx)
			if err == nil {
				for _, other := range models {
					if other.Channel == m.Channel && other.ID != m.ID && other.IsDefault {
						other.IsDefault = false
						s.models.UpdateModel(ctx, other)
					}
				}
			}
		}
		return s.models.UpdateModel(ctx, m)
	}
	return fmt.Errorf("models store not configured")
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
		models, err := s.models.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if m.ModelID == modelID {
				return m, nil
			}
		}
		return nil, fmt.Errorf("model not found")
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) ListModels(ctx context.Context) ([]*Model, error) {
	if s.models != nil {
		return s.models.ListModels(ctx)
	}
	return nil, fmt.Errorf("models store not configured")
}
