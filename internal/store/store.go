package store

import (
	"database/sql"
	"errors"
	"fmt"
	"orchids-api/internal/model"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Account struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	SessionID    string    `json:"session_id"`
	ClientCookie string    `json:"client_cookie"`
	ClientUat    string    `json:"client_uat"`
	ProjectID    string    `json:"project_id"`
	UserID       string    `json:"user_id"`
	AgentMode    string    `json:"agent_mode"`
	Email        string    `json:"email"`
	Weight       int       `json:"weight"`
	Enabled      bool      `json:"enabled"`
	Token        string    `json:"token"`        // Truncated display token
	Subscription string    `json:"subscription"` // "free", "pro", etc.
	UsageCurrent float64   `json:"usage_current"`
	UsageTotal   float64   `json:"usage_total"`
	ResetDate    string    `json:"reset_date"`
	RequestCount int64     `json:"request_count"`
	LastUsedAt   time.Time `json:"last_used_at"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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
	db       *sql.DB
	mu       sync.RWMutex
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
	CreateAccount(acc *Account) error
	UpdateAccount(acc *Account) error
	DeleteAccount(id int64) error
	GetAccount(id int64) (*Account, error)
	ListAccounts() ([]*Account, error)
	GetEnabledAccounts() ([]*Account, error)
	IncrementRequestCount(id int64) error
}

type settingsStore interface {
	GetSetting(key string) (string, error)
	SetSetting(key, value string) error
}

type apiKeyStore interface {
	CreateApiKey(key *ApiKey) error
	ListApiKeys() ([]*ApiKey, error)
	GetApiKeyByHash(hash string) (*ApiKey, error)
	UpdateApiKeyEnabled(id int64, enabled bool) error
	UpdateApiKeyLastUsed(id int64) error
	DeleteApiKey(id int64) error
	GetApiKeyByID(id int64) (*ApiKey, error)
}

type modelStore interface {
	CreateModel(m *model.Model) error
	UpdateModel(m *model.Model) error
	DeleteModel(id string) error
	GetModel(id string) (*model.Model, error)
	ListModels() ([]*model.Model, error)
}

type closeableStore interface {
	Close() error
}

func New(dbPath string, opts Options) (*Store, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.StoreMode))
	if mode == "" {
		mode = "redis"
	}

	store := &Store{}
	if mode == "redis" {
		redisStore, err := newRedisStore(opts.RedisAddr, opts.RedisPassword, opts.RedisDB, opts.RedisPrefix)
		if err != nil {
			return nil, fmt.Errorf("failed to init redis store: %w", err)
		}
		store.accounts = redisStore
		store.settings = redisStore
		store.apiKeys = redisStore
		store.models = redisStore
		return store, nil
	}

	if dbPath == "" {
		return nil, errors.New("sqlite db path is required when store_mode=sqlite")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	if err := applySQLitePragmas(db); err != nil {
		return nil, fmt.Errorf("failed to apply sqlite pragmas: %w", err)
	}

	store.db = db
	if err := store.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	return store, nil
}

func (s *Store) migrate() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			session_id TEXT NOT NULL,
			client_cookie TEXT NOT NULL,
			client_uat TEXT NOT NULL,
			project_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			agent_mode TEXT DEFAULT 'claude-opus-4.5',
			email TEXT NOT NULL,
			weight INTEGER DEFAULT 1,
			enabled INTEGER DEFAULT 1,
			token TEXT DEFAULT '',
			subscription TEXT DEFAULT 'free',
			usage_current REAL DEFAULT 0,
			usage_total REAL DEFAULT 550,
			reset_date TEXT DEFAULT '-',
			request_count INTEGER DEFAULT 0,
			last_used_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT UNIQUE NOT NULL,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_full TEXT NOT NULL DEFAULT '',
			key_prefix TEXT NOT NULL DEFAULT 'sk-',
			key_suffix TEXT NOT NULL,
			enabled INTEGER DEFAULT 1,
			last_used_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_enabled ON accounts(enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_enabled ON api_keys(enabled)`,
	}

	for _, q := range queries {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}

	// 迁移：为现有表添加新列
	s.db.Exec(`ALTER TABLE api_keys ADD COLUMN key_full TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE accounts ADD COLUMN token TEXT DEFAULT ''`)
	s.db.Exec(`ALTER TABLE accounts ADD COLUMN subscription TEXT DEFAULT 'free'`)
	s.db.Exec(`ALTER TABLE accounts ADD COLUMN usage_current REAL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE accounts ADD COLUMN usage_total REAL DEFAULT 550`)
	s.db.Exec(`ALTER TABLE accounts ADD COLUMN reset_date TEXT DEFAULT '-'`)

	return nil
}

func applySQLitePragmas(db *sql.DB) error {
	queries := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s.accounts != nil {
		if closer, ok := s.accounts.(closeableStore); ok {
			_ = closer.Close()
		}
	}
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) CreateAccount(acc *Account) error {
	if s.accounts != nil {
		return s.accounts.CreateAccount(acc)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(`
		INSERT INTO accounts (name, session_id, client_cookie, client_uat, project_id, user_id, agent_mode, email, weight, enabled, token, subscription, usage_current, usage_total, reset_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, acc.Name, acc.SessionID, acc.ClientCookie, acc.ClientUat, acc.ProjectID, acc.UserID, acc.AgentMode, acc.Email, acc.Weight, acc.Enabled, acc.Token, acc.Subscription, acc.UsageCurrent, acc.UsageTotal, acc.ResetDate)
	if err != nil {
		return err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	acc.ID = id
	return nil
}

func (s *Store) UpdateAccount(acc *Account) error {
	if s.accounts != nil {
		return s.accounts.UpdateAccount(acc)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE accounts SET
			name = ?, session_id = ?, client_cookie = ?, client_uat = ?,
			project_id = ?, user_id = ?, agent_mode = ?, email = ?,
			weight = ?, enabled = ?, token = ?, subscription = ?,
			usage_current = ?, usage_total = ?, reset_date = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, acc.Name, acc.SessionID, acc.ClientCookie, acc.ClientUat, acc.ProjectID, acc.UserID, acc.AgentMode, acc.Email, acc.Weight, acc.Enabled, acc.Token, acc.Subscription, acc.UsageCurrent, acc.UsageTotal, acc.ResetDate, acc.ID)
	return err
}

func (s *Store) DeleteAccount(id int64) error {
	if s.accounts != nil {
		return s.accounts.DeleteAccount(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM accounts WHERE id = ?", id)
	return err
}

func (s *Store) GetAccount(id int64) (*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetAccount(id)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	acc := &Account{}
	var lastUsedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, name, session_id, client_cookie, client_uat, project_id, user_id,
			   agent_mode, email, weight, enabled, token, subscription, usage_current, usage_total, reset_date,
			   request_count, last_used_at, created_at, updated_at
		FROM accounts WHERE id = ?
	`, id).Scan(&acc.ID, &acc.Name, &acc.SessionID, &acc.ClientCookie, &acc.ClientUat,
		&acc.ProjectID, &acc.UserID, &acc.AgentMode, &acc.Email, &acc.Weight,
		&acc.Enabled, &acc.Token, &acc.Subscription, &acc.UsageCurrent, &acc.UsageTotal, &acc.ResetDate,
		&acc.RequestCount, &lastUsedAt, &acc.CreatedAt, &acc.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		acc.LastUsedAt = lastUsedAt.Time
	}
	return acc, nil
}

func (s *Store) ListAccounts() ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.ListAccounts()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, name, session_id, client_cookie, client_uat, project_id, user_id,
			   agent_mode, email, weight, enabled, token, subscription, usage_current, usage_total, reset_date,
			   request_count, last_used_at, created_at, updated_at
		FROM accounts ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []*Account
	for rows.Next() {
		acc := &Account{}
		var lastUsedAt sql.NullTime
		err := rows.Scan(&acc.ID, &acc.Name, &acc.SessionID, &acc.ClientCookie, &acc.ClientUat,
			&acc.ProjectID, &acc.UserID, &acc.AgentMode, &acc.Email, &acc.Weight,
			&acc.Enabled, &acc.Token, &acc.Subscription, &acc.UsageCurrent, &acc.UsageTotal, &acc.ResetDate,
			&acc.RequestCount, &lastUsedAt, &acc.CreatedAt, &acc.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if lastUsedAt.Valid {
			acc.LastUsedAt = lastUsedAt.Time
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

func (s *Store) GetEnabledAccounts() ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetEnabledAccounts()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, name, session_id, client_cookie, client_uat, project_id, user_id,
			   agent_mode, email, weight, enabled, token, subscription, usage_current, usage_total, reset_date,
			   request_count, last_used_at, created_at, updated_at
		FROM accounts WHERE enabled = 1 ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []*Account
	for rows.Next() {
		acc := &Account{}
		var lastUsedAt sql.NullTime
		err := rows.Scan(&acc.ID, &acc.Name, &acc.SessionID, &acc.ClientCookie, &acc.ClientUat,
			&acc.ProjectID, &acc.UserID, &acc.AgentMode, &acc.Email, &acc.Weight,
			&acc.Enabled, &acc.Token, &acc.Subscription, &acc.UsageCurrent, &acc.UsageTotal, &acc.ResetDate,
			&acc.RequestCount, &lastUsedAt, &acc.CreatedAt, &acc.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if lastUsedAt.Valid {
			acc.LastUsedAt = lastUsedAt.Time
		}
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

func (s *Store) IncrementRequestCount(id int64) error {
	if s.accounts != nil {
		return s.accounts.IncrementRequestCount(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE accounts SET request_count = request_count + 1, last_used_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, id)
	return err
}

func (s *Store) GetSetting(key string) (string, error) {
	if s.settings != nil {
		return s.settings.GetSetting(key)
	}
	if s.db == nil {
		return "", errors.New("settings store not configured")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var value string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) SetSetting(key, value string) error {
	if s.settings != nil {
		return s.settings.SetSetting(key, value)
	}
	if s.db == nil {
		return errors.New("settings store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

func (s *Store) CreateApiKey(key *ApiKey) error {
	if s.apiKeys != nil {
		return s.apiKeys.CreateApiKey(key)
	}
	if s.db == nil {
		return errors.New("api keys store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(`
		INSERT INTO api_keys (name, key_hash, key_full, key_prefix, key_suffix, enabled)
		VALUES (?, ?, ?, ?, ?, ?)
	`, key.Name, key.KeyHash, key.KeyFull, key.KeyPrefix, key.KeySuffix, key.Enabled)
	if err != nil {
		return err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	key.ID = id

	var createdAt time.Time
	var lastUsedAt sql.NullTime
	if err := s.db.QueryRow(`
		SELECT enabled, last_used_at, created_at
		FROM api_keys WHERE id = ?
	`, id).Scan(&key.Enabled, &lastUsedAt, &createdAt); err != nil {
		return err
	}
	if lastUsedAt.Valid {
		t := lastUsedAt.Time
		key.LastUsedAt = &t
	} else {
		key.LastUsedAt = nil
	}
	key.CreatedAt = createdAt

	return nil
}

func (s *Store) ListApiKeys() ([]*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.ListApiKeys()
	}
	if s.db == nil {
		return nil, errors.New("api keys store not configured")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, name, key_full, key_prefix, key_suffix, enabled, last_used_at, created_at
		FROM api_keys ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*ApiKey
	for rows.Next() {
		key := &ApiKey{}
		var lastUsedAt sql.NullTime
		if err := rows.Scan(&key.ID, &key.Name, &key.KeyFull, &key.KeyPrefix, &key.KeySuffix, &key.Enabled, &lastUsedAt, &key.CreatedAt); err != nil {
			return nil, err
		}
		if lastUsedAt.Valid {
			t := lastUsedAt.Time
			key.LastUsedAt = &t
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return keys, nil
}

func (s *Store) GetApiKeyByHash(hash string) (*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.GetApiKeyByHash(hash)
	}
	if s.db == nil {
		return nil, errors.New("api keys store not configured")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := &ApiKey{}
	var lastUsedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, name, key_hash, key_prefix, key_suffix, enabled, last_used_at, created_at
		FROM api_keys WHERE key_hash = ?
	`, hash).Scan(&key.ID, &key.Name, &key.KeyHash, &key.KeyPrefix, &key.KeySuffix, &key.Enabled, &lastUsedAt, &key.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		t := lastUsedAt.Time
		key.LastUsedAt = &t
	}
	return key, nil
}

func (s *Store) UpdateApiKeyEnabled(id int64, enabled bool) error {
	if s.apiKeys != nil {
		return s.apiKeys.UpdateApiKeyEnabled(id, enabled)
	}
	if s.db == nil {
		return errors.New("api keys store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec(`
		UPDATE api_keys SET enabled = ?
		WHERE id = ?
	`, enabled, id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateApiKeyLastUsed(id int64) error {
	if s.apiKeys != nil {
		return s.apiKeys.UpdateApiKeyLastUsed(id)
	}
	if s.db == nil {
		return errors.New("api keys store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		UPDATE api_keys SET last_used_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, id)
	return err
}

func (s *Store) DeleteApiKey(id int64) error {
	if s.apiKeys != nil {
		return s.apiKeys.DeleteApiKey(id)
	}
	if s.db == nil {
		return errors.New("api keys store not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.Exec("DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetApiKeyByID(id int64) (*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.GetApiKeyByID(id)
	}
	if s.db == nil {
		return nil, errors.New("api keys store not configured")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := &ApiKey{}
	var lastUsedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, name, key_prefix, key_suffix, enabled, last_used_at, created_at
		FROM api_keys WHERE id = ?
	`, id).Scan(&key.ID, &key.Name, &key.KeyPrefix, &key.KeySuffix, &key.Enabled, &lastUsedAt, &key.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		t := lastUsedAt.Time
		key.LastUsedAt = &t
	}
	return key, nil
}

// Model wrappers

func (s *Store) CreateModel(m *model.Model) error {
	if s.models != nil {
		return s.models.CreateModel(m)
	}
	return errors.New("sqlite store for models not implemented")
}

func (s *Store) UpdateModel(m *model.Model) error {
	if s.models != nil {
		return s.models.UpdateModel(m)
	}
	return errors.New("sqlite store for models not implemented")
}

func (s *Store) DeleteModel(id string) error {
	if s.models != nil {
		return s.models.DeleteModel(id)
	}
	return errors.New("sqlite store for models not implemented")
}

func (s *Store) GetModel(id string) (*model.Model, error) {
	if s.models != nil {
		return s.models.GetModel(id)
	}
	return nil, errors.New("sqlite store for models not implemented")
}

func (s *Store) ListModels() ([]*model.Model, error) {
	if s.models != nil {
		return s.models.ListModels()
	}
	return nil, errors.New("sqlite store for models not implemented")
}
