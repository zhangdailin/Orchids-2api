package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/util"

	"github.com/redis/go-redis/v9"
)

type redisStore struct {
	client      *redis.Client
	prefix      string
	credentials *credentialCipher
}

const redisBatchParallelThreshold = 32

var (
	consumeApiKeyRPMScript = redis.NewScript(`
		local value = redis.call("GET", KEYS[2])
		if not value then return redis.error_reply("api key not found") end
		local api_key = cjson.decode(value)
		api_key.last_used_at = ARGV[2]
		redis.call("SET", KEYS[2], cjson.encode(api_key))
		local limit = tonumber(ARGV[3])
		if limit <= 0 then return 0 end
		local count = redis.call("INCR", KEYS[1])
		if count == 1 then
			redis.call("EXPIRE", KEYS[1], tonumber(ARGV[1]))
		end
		return count
	`)
	incrementAccountStatsScript = redis.NewScript(`
		local key = KEYS[1]
		local usage = tonumber(ARGV[1])
		local count = tonumber(ARGV[2])
		local now_str = ARGV[3]
		local val = redis.call("GET", key)
		if not val then return redis.error_reply("account not found") end
		local acc = cjson.decode(val)
		local acc_type = ""
		if acc.account_type ~= nil then
			acc_type = string.lower(tostring(acc.account_type))
		end
		if acc_type ~= "warp" and acc_type ~= "puter" and acc_type ~= "grok" then
			acc.usage_current = (acc.usage_current or 0) + usage
		end
		acc.usage_total = (acc.usage_total or 0) + usage
		acc.request_count = (acc.request_count or 0) + count
		acc.last_used_at = now_str
		acc.updated_at = now_str
		redis.call("SET", key, cjson.encode(acc))
		return "OK"
	`)
	refreshVideoJobLeaseScript = redis.NewScript(`
		if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
		redis.call("PEXPIRE", KEYS[1], ARGV[2])
		return 1
	`)
	releaseVideoJobLeaseScript = redis.NewScript(`
		if redis.call("GET", KEYS[1]) ~= ARGV[1] then return 0 end
		return redis.call("DEL", KEYS[1])
	`)
)

type apiKeyRecord struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	KeyHash       string     `json:"key_hash"`
	KeyFull       string     `json:"key_full,omitempty"`
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

func newRedisStore(addr, password string, db int, prefix string, credentialKey []byte) (*redisStore, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, fmt.Errorf("redis address is required")
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "orchids:"
	}
	if !strings.HasSuffix(prefix, ":") {
		prefix += ":"
	}

	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     200,
		MinIdleConns: 20,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	credentials, err := newCredentialCipher(credentialKey)
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return &redisStore{client: client, prefix: prefix, credentials: credentials}, nil
}

func (s *redisStore) Client() *redis.Client {
	if s == nil {
		return nil
	}
	return s.client
}

func (s *redisStore) Close() error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Close()
}

func (s *redisStore) CreateAccount(ctx context.Context, acc *Account) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	id, err := s.client.Incr(ctx, s.accountsNextIDKey()).Result()
	if err != nil {
		return err
	}

	now := time.Now()
	acc.ID = id
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = now
	}
	if acc.UpdatedAt.IsZero() {
		acc.UpdatedAt = now
	}

	data, err := s.marshalAccount(acc)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.accountsKey(id), data, 0)
	pipe.SAdd(ctx, s.accountsIDsKey(), id)
	if acc.Enabled {
		pipe.SAdd(ctx, s.accountsEnabledKey(), id)
	} else {
		pipe.SRem(ctx, s.accountsEnabledKey(), id)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) UpdateAccount(ctx context.Context, acc *Account) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if acc.ID == 0 {
		return nil
	}

	existing, err := s.getAccount(ctx, acc.ID)
	if err == ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	updated := *existing
	updated.Name = acc.Name
	if acc.AccountType == "" {
		updated.AccountType = existing.AccountType
	} else {
		updated.AccountType = acc.AccountType
	}
	updated.NSFWEnabled = acc.NSFWEnabled
	updated.SessionID = acc.SessionID
	updated.ClientCookie = acc.ClientCookie
	if strings.EqualFold(updated.AccountType, "warp") {
		if strings.TrimSpace(acc.RefreshToken) == "" {
			updated.RefreshToken = existing.RefreshToken
		} else {
			updated.RefreshToken = acc.RefreshToken
		}
		if strings.TrimSpace(acc.DeviceID) == "" {
			updated.DeviceID = existing.DeviceID
		} else {
			updated.DeviceID = acc.DeviceID
		}
		if strings.TrimSpace(acc.RequestID) == "" {
			updated.RequestID = existing.RequestID
		} else {
			updated.RequestID = acc.RequestID
		}
	} else {
		updated.RefreshToken = acc.RefreshToken
		updated.DeviceID = acc.DeviceID
		updated.RequestID = acc.RequestID
	}
	if strings.EqualFold(updated.AccountType, "warp") {
		updated.SessionCookie = ""
	} else if acc.SessionCookie == "" {
		updated.SessionCookie = existing.SessionCookie
	} else {
		updated.SessionCookie = acc.SessionCookie
	}
	updated.ClientUat = acc.ClientUat
	updated.ProjectID = acc.ProjectID
	updated.UserID = acc.UserID
	updated.AgentMode = acc.AgentMode
	updated.Email = acc.Email
	updated.Weight = acc.Weight
	updated.MaxConcurrent = acc.MaxConcurrent
	updated.Enabled = acc.Enabled
	updated.Token = acc.Token
	updated.Subscription = acc.Subscription
	updated.UsageCurrent = acc.UsageCurrent
	updated.UsageTotal = acc.UsageTotal
	updated.UsageLimit = acc.UsageLimit
	updated.WarpMonthlyLimit = acc.WarpMonthlyLimit
	updated.WarpMonthlyRemaining = acc.WarpMonthlyRemaining
	updated.WarpBonusRemaining = acc.WarpBonusRemaining
	updated.StatusCode = acc.StatusCode
	// The reason describes the CURRENT status only. It must never outlive the
	// status it explains, or a recovered account keeps showing a stale error.
	if strings.TrimSpace(acc.StatusCode) == "" {
		updated.StatusMessage = ""
	} else {
		updated.StatusMessage = acc.StatusMessage
	}
	updated.LastAttempt = acc.LastAttempt
	updated.QuotaResetAt = acc.QuotaResetAt
	updated.MissingThinkingStrikes = acc.MissingThinkingStrikes
	updated.MissingThinkingLastAt = acc.MissingThinkingLastAt
	// Grok Build CLI OAuth credentials and identity must survive refresh /
	// admin updates. Leaving these out would silently drop rotated tokens.
	if strings.TrimSpace(acc.CredentialType) == "" {
		updated.CredentialType = existing.CredentialType
	} else {
		updated.CredentialType = acc.CredentialType
	}
	updated.OAuthAccessToken = acc.OAuthAccessToken
	updated.OAuthRefreshToken = acc.OAuthRefreshToken
	updated.OAuthExpiresAt = acc.OAuthExpiresAt
	if strings.TrimSpace(acc.TeamID) == "" {
		updated.TeamID = existing.TeamID
	} else {
		updated.TeamID = acc.TeamID
	}
	if strings.TrimSpace(acc.UpstreamMode) == "" {
		updated.UpstreamMode = existing.UpstreamMode
	} else {
		updated.UpstreamMode = acc.UpstreamMode
	}
	if strings.TrimSpace(acc.GrokProvider) == "" {
		updated.GrokProvider = existing.GrokProvider
	} else {
		updated.GrokProvider = acc.GrokProvider
	}
	if acc.GrokSSOParentID == 0 {
		updated.GrokSSOParentID = existing.GrokSSOParentID
	} else {
		updated.GrokSSOParentID = acc.GrokSSOParentID
	}
	// Account updates are often partial (for example request counters and
	// credential rotation). Provider snapshots are refreshed independently, so
	// never erase a successfully observed catalog/billing window with a zero
	// value from an unrelated update.
	if acc.GrokModels != nil {
		updated.GrokModels = append([]string(nil), acc.GrokModels...)
	}
	if !acc.GrokModelsSyncedAt.IsZero() {
		updated.GrokModelsSyncedAt = acc.GrokModelsSyncedAt
	}
	if !acc.GrokBilling.SyncedAt.IsZero() {
		updated.GrokBilling = acc.GrokBilling
	}
	if !acc.GrokRateLimits.ObservedAt.IsZero() {
		updated.GrokRateLimits = acc.GrokRateLimits
	}
	if !acc.GrokWebQuota.SyncedAt.IsZero() {
		updated.GrokWebQuota = acc.GrokWebQuota
	}
	// WorkBuddy credentials are rotated by the upstream (Keycloak rotates the
	// refresh token on every renewal) and account updates are frequently
	// partial, so an empty value means "keep what is stored", never "erase".
	if token := strings.TrimSpace(acc.WorkBuddyAccessToken); token != "" {
		updated.WorkBuddyAccessToken = token
	}
	if token := strings.TrimSpace(acc.WorkBuddyRefreshToken); token != "" {
		updated.WorkBuddyRefreshToken = token
	}
	if token := strings.TrimSpace(acc.WorkBuddyUID); token != "" {
		updated.WorkBuddyUID = token
	}
	if !acc.WorkBuddyExpiresAt.IsZero() {
		updated.WorkBuddyExpiresAt = acc.WorkBuddyExpiresAt
	}
	if len(acc.WorkBuddyModelIDs) > 0 {
		updated.WorkBuddyModelIDs = append([]string(nil), acc.WorkBuddyModelIDs...)
	}
	if !acc.WorkBuddyModelsSyncedAt.IsZero() {
		updated.WorkBuddyModelsSyncedAt = acc.WorkBuddyModelsSyncedAt
	}
	if !acc.WorkBuddyQuota.SyncedAt.IsZero() {
		updated.WorkBuddyQuota = acc.WorkBuddyQuota
	}
	updated.UpdatedAt = time.Now()

	data, err := s.marshalAccount(&updated)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.accountsKey(acc.ID), data, 0)
	pipe.SAdd(ctx, s.accountsIDsKey(), acc.ID)
	if updated.Enabled {
		pipe.SAdd(ctx, s.accountsEnabledKey(), acc.ID)
	} else {
		pipe.SRem(ctx, s.accountsEnabledKey(), acc.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) DeleteAccount(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.accountsKey(id))
	pipe.SRem(ctx, s.accountsIDsKey(), id)
	pipe.SRem(ctx, s.accountsEnabledKey(), id)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetAccount(ctx context.Context, id int64) (*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	return s.getAccount(ctx, id)
}

func (s *redisStore) ListAccounts(ctx context.Context) ([]*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.accountsIDsKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getAccountsByIDs(ctx, ids, false)
}

func (s *redisStore) GetEnabledAccounts(ctx context.Context) ([]*Account, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.accountsEnabledKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getAccountsByIDs(ctx, ids, true)
}

func (s *redisStore) IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}
	if usage <= 0 && count <= 0 {
		return nil
	}
	nowStr := time.Now().Format(time.RFC3339Nano)
	keys := []string{s.accountsKey(id)}
	args := []interface{}{usage, count, nowStr}

	err := incrementAccountStatsScript.Run(ctx, s.client, keys, args...).Err()
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

func (s *redisStore) getAccount(ctx context.Context, id int64) (*Account, error) {
	if id == 0 {
		return nil, ErrNoRows
	}
	value, err := s.client.Get(ctx, s.accountsKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	return s.unmarshalAccount([]byte(value), id)
}

// getAccountsByIDsPipelined 使用 Pipeline 批量获取账号数据
func (s *redisStore) getAccountsByIDsPipelined(ctx context.Context, keys []string) ([]interface{}, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := s.client.Pipeline()
	cmds := make([]*redis.StringCmd, len(keys))

	// 批量添加 GET 命令到 Pipeline
	for i, key := range keys {
		cmds[i] = pipe.Get(ctx, key)
	}

	// 执行 Pipeline
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, err
	}

	// 收集结果
	values := make([]interface{}, len(cmds))
	for i, cmd := range cmds {
		val, err := cmd.Result()
		if err == redis.Nil {
			values[i] = nil
		} else if err != nil {
			// 部分命令失败，返回错误触发回退
			return nil, err
		} else {
			values[i] = val
		}
	}

	return values, nil
}

func (s *redisStore) getAccountsByIDs(ctx context.Context, ids []string, onlyEnabled bool) ([]*Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idNums := parseSortedInt64s(ids)
	if len(idNums) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.accountsKey(id))
	}

	// 尝试使用 Pipeline 批量获取
	values, err := s.getAccountsByIDsPipelined(ctx, keys)
	if err != nil {
		// Pipeline 失败，回退到单命令模式
		values, err = s.client.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
	}

	results := make([]*Account, len(values))
	decodeErrs := make([]error, len(values))
	decode := func(i int) {
		strVal, ok := values[i].(string)
		if !ok || strVal == "" {
			return
		}
		acc, err := s.unmarshalAccount([]byte(strVal), idNums[i])
		if err != nil {
			decodeErrs[i] = err
			return
		}
		if onlyEnabled && !acc.Enabled {
			return
		}
		results[i] = acc
	}
	if len(values) >= redisBatchParallelThreshold {
		util.ParallelFor(len(values), decode)
	} else {
		for i := range values {
			decode(i)
		}
	}
	for _, decodeErr := range decodeErrs {
		if decodeErr != nil {
			return nil, decodeErr
		}
	}

	accounts := make([]*Account, 0, len(values))
	for _, acc := range results {
		if acc != nil {
			accounts = append(accounts, acc)
		}
	}
	return accounts, nil
}

func (s *redisStore) GetSetting(ctx context.Context, key string) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("redis store not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	value, err := s.client.Get(ctx, s.settingsKey(key)).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *redisStore) SetSetting(ctx context.Context, key, value string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return s.client.Set(ctx, s.settingsKey(key), value, 0).Err()
}

func (s *redisStore) CreateApiKey(ctx context.Context, key *ApiKey) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	id, err := s.client.Incr(ctx, s.apiKeysNextIDKey()).Result()
	if err != nil {
		return err
	}

	now := time.Now()
	key.ID = id
	if key.CreatedAt.IsZero() {
		key.CreatedAt = now
	}

	record := apiKeyRecordFromKey(key)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.apiKeysKey(id), data, 0)
	pipe.SAdd(ctx, s.apiKeysIDsKey(), id)
	if record.KeyHash != "" {
		pipe.Set(ctx, s.apiKeysHashKey(record.KeyHash), id, 0)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) ListApiKeys(ctx context.Context) ([]*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.apiKeysIDsKey()).Result()
	if err != nil {
		return nil, err
	}
	return s.getApiKeysByIDs(ctx, ids)
}

func (s *redisStore) UpdateApiKey(ctx context.Context, key *ApiKey) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if key == nil || key.ID == 0 {
		return ErrNoRows
	}
	existing, err := s.getApiKeyByID(ctx, key.ID)
	if err != nil {
		return err
	}
	record := apiKeyRecordFromKey(key)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.apiKeysKey(key.ID), data, 0)
	if existing.KeyHash != record.KeyHash {
		if existing.KeyHash != "" {
			pipe.Del(ctx, s.apiKeysHashKey(existing.KeyHash))
		}
		if record.KeyHash != "" {
			pipe.Set(ctx, s.apiKeysHashKey(record.KeyHash), key.ID, 0)
		}
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) DeleteApiKey(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.apiKeysKey(id))
	pipe.SRem(ctx, s.apiKeysIDsKey(), id)
	if key.KeyHash != "" {
		pipe.Del(ctx, s.apiKeysHashKey(key.KeyHash))
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	return s.getApiKeyByID(ctx, id)
}

func (s *redisStore) GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, ErrNoRows
	}
	id, err := s.client.Get(ctx, s.apiKeysHashKey(hash)).Int64()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return s.getApiKeyByID(ctx, id)
}

func (s *redisStore) ConsumeApiKeyRPM(ctx context.Context, id int64, limit int, now time.Time) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return false, ErrNoRows
	}
	minute := now.UTC().Unix() / 60
	ttl := int64(120)
	count, err := consumeApiKeyRPMScript.Run(
		ctx,
		s.client,
		[]string{s.apiKeyRPMKey(id, minute), s.apiKeysKey(id)},
		ttl,
		now.UTC().Format(time.RFC3339Nano),
		limit,
	).Int64()
	if err != nil {
		return false, err
	}
	return count <= int64(limit), nil
}

func (s *redisStore) getApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if id == 0 {
		return nil, ErrNoRows
	}
	value, err := s.client.Get(ctx, s.apiKeysKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var record apiKeyRecord
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return nil, err
	}
	key := record.toApiKey()
	if key.ID == 0 {
		key.ID = id
	}
	return key, nil
}

func (s *redisStore) getApiKeysByIDs(ctx context.Context, ids []string) ([]*ApiKey, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idNums := parseSortedInt64s(ids)
	if len(idNums) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.apiKeysKey(id))
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	results := make([]*ApiKey, len(values))
	decode := func(i int) {
		strVal, ok := values[i].(string)
		if !ok || strVal == "" {
			return
		}
		var record apiKeyRecord
		if err := json.Unmarshal([]byte(strVal), &record); err != nil {
			return
		}
		key := record.toApiKey()
		if key.ID == 0 {
			key.ID = idNums[i]
		}
		results[i] = key
	}
	if len(values) >= redisBatchParallelThreshold {
		util.ParallelFor(len(values), decode)
	} else {
		for i := range values {
			decode(i)
		}
	}

	items := make([]*ApiKey, 0, len(values))
	for _, key := range results {
		if key != nil {
			items = append(items, key)
		}
	}
	return items, nil
}

func parseSortedInt64s(values []string) []int64 {
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *redisStore) accountsKey(id int64) string {
	return fmt.Sprintf("%saccounts:id:%d", s.prefix, id)
}

func (s *redisStore) accountsIDsKey() string {
	return s.prefix + "accounts:ids"
}

func (s *redisStore) accountsEnabledKey() string {
	return s.prefix + "accounts:enabled"
}

func (s *redisStore) accountsNextIDKey() string {
	return s.prefix + "accounts:next_id"
}

func (s *redisStore) settingsKey(key string) string {
	return s.prefix + "settings:" + key
}

func (s *redisStore) apiKeysKey(id int64) string {
	return fmt.Sprintf("%sapi_keys:id:%d", s.prefix, id)
}

func (s *redisStore) apiKeysIDsKey() string {
	return s.prefix + "api_keys:ids"
}

func (s *redisStore) apiKeysNextIDKey() string {
	return s.prefix + "api_keys:next_id"
}

func (s *redisStore) apiKeysHashKey(hash string) string {
	return s.prefix + "api_keys:hash:" + hash
}

func (s *redisStore) apiKeyRPMKey(id, minute int64) string {
	return fmt.Sprintf("%sapi_keys:rpm:%d:%d", s.prefix, id, minute)
}

func (s *redisStore) storedResponseKey(responseID, ownerHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ownerHash) + "\x00" + strings.TrimSpace(responseID)))
	return s.prefix + "responses:ownership:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) storedVideoJobKey(id, ownerHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ownerHash) + "\x00" + strings.TrimSpace(id)))
	return s.prefix + "videos:jobs:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) storedVideoJobsIndexKey() string {
	return s.prefix + "videos:jobs:index"
}

func (s *redisStore) storedVideoJobLeaseKey(id, ownerHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ownerHash) + "\x00" + strings.TrimSpace(id)))
	return s.prefix + "videos:leases:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) storedMediaInputKey(id, ownerHash string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ownerHash) + "\x00" + strings.TrimSpace(id)))
	return s.prefix + "media:inputs:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if response == nil || strings.TrimSpace(response.ResponseID) == "" || strings.TrimSpace(response.OwnerHash) == "" {
		return fmt.Errorf("response id and owner are required")
	}
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	now := time.Now().UTC()
	stored := *response
	stored.ResponseID = strings.TrimSpace(stored.ResponseID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	stored.UpdatedAt = now
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.storedResponseKey(stored.ResponseID, stored.OwnerHash), data, ttl).Err()
}

func (s *redisStore) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	key := s.storedResponseKey(responseID, ownerHash)
	return getExpiringRedisJSON[StoredResponse](ctx, s, key, func(response *StoredResponse) time.Time {
		return response.ExpiresAt
	})
}

func getExpiringRedisJSON[T any](ctx context.Context, s *redisStore, key string, expiresAt func(*T) time.Time) (*T, error) {
	value, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(value, &result); err != nil {
		return nil, err
	}
	expiry := expiresAt(&result)
	if !expiry.IsZero() && !time.Now().UTC().Before(expiry) {
		_ = s.client.Del(ctx, key).Err()
		return nil, ErrNoRows
	}
	return &result, nil
}

func (s *redisStore) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	deleted, err := s.client.Del(ctx, s.storedResponseKey(responseID, ownerHash)).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNoRows
	}
	return nil
}

func (s *redisStore) reasoningReplayKey(model, sessionKey string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimSpace(sessionKey)))
	return s.prefix + "grok:reasoning_replay:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	return s.client.Del(ctx, s.reasoningReplayKey(model, key)).Err()
}

func (s *redisStore) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if replay == nil || strings.TrimSpace(replay.Model) == "" || strings.TrimSpace(replay.SessionKey) == "" || strings.TrimSpace(replay.EncryptedContent) == "" {
		return fmt.Errorf("invalid reasoning replay")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *replay
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	raw, err := json.Marshal(&next)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.reasoningReplayKey(next.Model, next.SessionKey), raw, ttl).Err()
}

func (s *redisStore) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	raw, err := s.client.Get(ctx, s.reasoningReplayKey(model, sessionKey)).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var replay StoredReasoningReplay
	if err := json.Unmarshal(raw, &replay); err != nil {
		return nil, err
	}
	if !replay.ExpiresAt.IsZero() && !time.Now().UTC().Before(replay.ExpiresAt) {
		_ = s.client.Del(ctx, s.reasoningReplayKey(model, sessionKey)).Err()
		return nil, ErrNoRows
	}
	return &replay, nil
}

func (s *redisStore) puterReasoningReplayKey(model, toolCallID string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimSpace(toolCallID)))
	return s.prefix + "puter:reasoning_replay:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) SavePuterReasoningReplay(ctx context.Context, replay *StoredPuterReasoningReplay, ttl time.Duration) error {
	if replay == nil || strings.TrimSpace(replay.Model) == "" || strings.TrimSpace(replay.ToolCallID) == "" || strings.TrimSpace(replay.ReasoningContent) == "" {
		return fmt.Errorf("invalid puter reasoning replay")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *replay
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	encrypted, err := s.credentials.encrypt(next.ReasoningContent)
	if err != nil {
		return fmt.Errorf("encrypt puter reasoning replay: %w", err)
	}
	next.ReasoningContent = encrypted
	raw, err := json.Marshal(&next)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.puterReasoningReplayKey(next.Model, next.ToolCallID), raw, ttl).Err()
}

func (s *redisStore) GetPuterReasoningReplay(ctx context.Context, model, toolCallID string) (*StoredPuterReasoningReplay, error) {
	key := s.puterReasoningReplayKey(model, toolCallID)
	raw, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var replay StoredPuterReasoningReplay
	if err := json.Unmarshal(raw, &replay); err != nil {
		return nil, err
	}
	if !replay.ExpiresAt.IsZero() && !time.Now().UTC().Before(replay.ExpiresAt) {
		_ = s.client.Del(ctx, key).Err()
		return nil, ErrNoRows
	}
	plain, err := s.credentials.decrypt(replay.ReasoningContent)
	if err != nil {
		return nil, fmt.Errorf("decrypt puter reasoning replay: %w", err)
	}
	replay.ReasoningContent = plain
	return &replay, nil
}

func (s *redisStore) sessionAffinityKey(provider, model, sessionKey string) string {
	source := strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.ToLower(strings.TrimSpace(model)) + "\x00" + strings.TrimSpace(sessionKey)
	digest := sha256.Sum256([]byte(source))
	return s.prefix + "grok:session_affinity:" + hex.EncodeToString(digest[:])
}

func (s *redisStore) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if affinity == nil || strings.TrimSpace(affinity.Provider) == "" || strings.TrimSpace(affinity.Model) == "" || strings.TrimSpace(affinity.SessionKey) == "" || affinity.AccountID == 0 {
		return fmt.Errorf("invalid session affinity")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	next := *affinity
	next.ExpiresAt = time.Now().UTC().Add(ttl)
	raw, err := json.Marshal(&next)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.sessionAffinityKey(next.Provider, next.Model, next.SessionKey), raw, ttl).Err()
}

func (s *redisStore) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	key := s.sessionAffinityKey(provider, model, sessionKey)
	raw, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var affinity StoredSessionAffinity
	if err := json.Unmarshal(raw, &affinity); err != nil {
		return nil, err
	}
	if !affinity.ExpiresAt.IsZero() && !time.Now().UTC().Before(affinity.ExpiresAt) {
		_ = s.client.Del(ctx, key).Err()
		return nil, ErrNoRows
	}
	return &affinity, nil
}

func (s *redisStore) SaveStoredVideoJob(ctx context.Context, job *StoredVideoJob, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if job == nil || strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.OwnerHash) == "" {
		return fmt.Errorf("video job id and owner are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("video job ttl must be positive")
	}
	now := time.Now().UTC()
	stored := *job
	stored.ID = strings.TrimSpace(stored.ID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	stored.UpdatedAt = now
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	key := s.storedVideoJobKey(stored.ID, stored.OwnerHash)
	pipe := s.client.TxPipeline()
	pipe.Set(ctx, key, data, ttl)
	pipe.ZAdd(ctx, s.storedVideoJobsIndexKey(), redis.Z{Score: float64(stored.ExpiresAt.Unix()), Member: key})
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetStoredVideoJob(ctx context.Context, id, ownerHash string) (*StoredVideoJob, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	value, err := s.client.Get(ctx, s.storedVideoJobKey(id, ownerHash)).Bytes()
	if err == redis.Nil {
		return nil, ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	var job StoredVideoJob
	if err := json.Unmarshal(value, &job); err != nil {
		return nil, err
	}
	if !job.ExpiresAt.IsZero() && !time.Now().UTC().Before(job.ExpiresAt) {
		key := s.storedVideoJobKey(id, ownerHash)
		pipe := s.client.TxPipeline()
		pipe.Del(ctx, key)
		pipe.ZRem(ctx, s.storedVideoJobsIndexKey(), key)
		_, _ = pipe.Exec(ctx)
		return nil, ErrNoRows
	}
	return &job, nil
}

func (s *redisStore) ListStoredVideoJobs(ctx context.Context) ([]*StoredVideoJob, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	now := time.Now().UTC()
	indexKey := s.storedVideoJobsIndexKey()
	if err := s.client.ZRemRangeByScore(ctx, indexKey, "-inf", strconv.FormatInt(now.Unix(), 10)).Err(); err != nil {
		return nil, err
	}
	keys, err := s.client.ZRangeByScore(ctx, indexKey, &redis.ZRangeBy{Min: strconv.FormatInt(now.Unix()+1, 10), Max: "+inf"}).Result()
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	jobs := make([]*StoredVideoJob, 0, len(values))
	stale := make([]interface{}, 0)
	for index, value := range values {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			stale = append(stale, keys[index])
			continue
		}
		var job StoredVideoJob
		if json.Unmarshal([]byte(text), &job) != nil || (!job.ExpiresAt.IsZero() && !now.Before(job.ExpiresAt)) {
			stale = append(stale, keys[index])
			continue
		}
		jobs = append(jobs, &job)
	}
	if len(stale) > 0 {
		_ = s.client.ZRem(ctx, indexKey, stale...).Err()
	}
	return jobs, nil
}

func validateVideoJobLeaseArgs(id, ownerHash, holder string, ttl time.Duration) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ownerHash) == "" || strings.TrimSpace(holder) == "" {
		return fmt.Errorf("video job id, owner, and lease holder are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("video job lease ttl must be positive")
	}
	return nil
}

func (s *redisStore) AcquireVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if err := validateVideoJobLeaseArgs(id, ownerHash, holder, ttl); err != nil {
		return false, err
	}
	return s.client.SetNX(ctx, s.storedVideoJobLeaseKey(id, ownerHash), strings.TrimSpace(holder), ttl).Result()
}

func (s *redisStore) RefreshVideoJobLease(ctx context.Context, id, ownerHash, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if err := validateVideoJobLeaseArgs(id, ownerHash, holder, ttl); err != nil {
		return false, err
	}
	value, err := refreshVideoJobLeaseScript.Run(ctx, s.client, []string{s.storedVideoJobLeaseKey(id, ownerHash)}, strings.TrimSpace(holder), ttl.Milliseconds()).Int64()
	return value == 1, err
}

func (s *redisStore) ReleaseVideoJobLease(ctx context.Context, id, ownerHash, holder string) (bool, error) {
	if s == nil || s.client == nil {
		return false, fmt.Errorf("redis store not configured")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(ownerHash) == "" || strings.TrimSpace(holder) == "" {
		return false, fmt.Errorf("video job id, owner, and lease holder are required")
	}
	value, err := releaseVideoJobLeaseScript.Run(ctx, s.client, []string{s.storedVideoJobLeaseKey(id, ownerHash)}, strings.TrimSpace(holder)).Int64()
	return value == 1, err
}

func (s *redisStore) SaveStoredMediaInput(ctx context.Context, input *StoredMediaInput, ttl time.Duration) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if input == nil || strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.OwnerHash) == "" {
		return fmt.Errorf("media input id and owner are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("media input ttl must be positive")
	}
	now := time.Now().UTC()
	stored := *input
	stored.ID = strings.TrimSpace(stored.ID)
	stored.OwnerHash = strings.TrimSpace(stored.OwnerHash)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = now
	}
	stored.ExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.storedMediaInputKey(stored.ID, stored.OwnerHash), data, ttl).Err()
}

func (s *redisStore) GetStoredMediaInput(ctx context.Context, id, ownerHash string) (*StoredMediaInput, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	key := s.storedMediaInputKey(id, ownerHash)
	return getExpiringRedisJSON[StoredMediaInput](ctx, s, key, func(input *StoredMediaInput) time.Time {
		return input.ExpiresAt
	})
}

func (s *redisStore) DeleteStoredMediaInput(ctx context.Context, id, ownerHash string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	deleted, err := s.client.Del(ctx, s.storedMediaInputKey(id, ownerHash)).Result()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNoRows
	}
	return nil
}

func apiKeyRecordFromKey(key *ApiKey) apiKeyRecord {
	return apiKeyRecord{
		ID:            key.ID,
		Name:          key.Name,
		KeyHash:       key.KeyHash,
		KeyPrefix:     key.KeyPrefix,
		KeySuffix:     key.KeySuffix,
		Enabled:       key.Enabled,
		AllowedModels: append([]string(nil), key.AllowedModels...),
		RPMLimit:      key.RPMLimit,
		MaxConcurrent: key.MaxConcurrent,
		ExpiresAt:     key.ExpiresAt,
		LastUsedAt:    key.LastUsedAt,
		CreatedAt:     key.CreatedAt,
	}
}

func (r apiKeyRecord) toApiKey() *ApiKey {
	return &ApiKey{
		ID:            r.ID,
		Name:          r.Name,
		KeyHash:       r.KeyHash,
		KeyFull:       r.KeyFull,
		KeyPrefix:     r.KeyPrefix,
		KeySuffix:     r.KeySuffix,
		Enabled:       r.Enabled,
		AllowedModels: append([]string(nil), r.AllowedModels...),
		RPMLimit:      r.RPMLimit,
		MaxConcurrent: r.MaxConcurrent,
		ExpiresAt:     r.ExpiresAt,
		LastUsedAt:    r.LastUsedAt,
		CreatedAt:     r.CreatedAt,
	}
}

// Model wrappers

func (s *redisStore) CreateModel(ctx context.Context, m *Model) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}

	// Use a counter for ID generation to match screenshot style (numeric)
	id, err := s.client.Incr(ctx, s.modelsNextIDKey()).Result()
	if err != nil {
		return err
	}
	m.ID = strconv.FormatInt(id, 10)

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if strings.TrimSpace(m.ModelID) != "" {
		pipe.HSetNX(ctx, s.modelsModelIDMapKey(), m.ModelID, m.ID)
		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) UpdateModel(ctx context.Context, m *Model) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if m.ID == "" {
		return fmt.Errorf("model id is required")
	}

	prev, _ := s.GetModel(ctx, m.ID)

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
	if prev != nil && strings.TrimSpace(prev.ModelID) != "" {
		prevKey := modelChannelIndexKey(prev.Channel, prev.ModelID)
		nextKey := modelChannelIndexKey(m.Channel, m.ModelID)
		if prevKey != nextKey {
			pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), prevKey)
		}
	}
	if strings.TrimSpace(m.ModelID) != "" {
		currentGlobalID, _ := s.client.HGet(ctx, s.modelsModelIDMapKey(), m.ModelID).Result()
		if currentGlobalID == "" || currentGlobalID == m.ID || (prev != nil && currentGlobalID == prev.ID) {
			pipe.HSet(ctx, s.modelsModelIDMapKey(), m.ModelID, m.ID)
		}
		pipe.HSet(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID), m.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *redisStore) DeleteModel(ctx context.Context, id string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == "" {
		return nil
	}

	// Fetch model to get ModelID for index cleanup
	m, _ := s.GetModel(ctx, id)

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.modelsKey(id))
	pipe.SRem(ctx, s.modelsIDsKey(), id)
	if m != nil && strings.TrimSpace(m.ModelID) != "" {
		currentGlobalID, _ := s.client.HGet(ctx, s.modelsModelIDMapKey(), m.ModelID).Result()
		if currentGlobalID == id {
			pipe.HDel(ctx, s.modelsModelIDMapKey(), m.ModelID)
		}
		pipe.HDel(ctx, s.modelsChannelModelIDMapKey(), modelChannelIndexKey(m.Channel, m.ModelID))
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) GetModel(ctx context.Context, id string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	value, err := s.client.Get(ctx, s.modelsKey(id)).Result()
	if err == redis.Nil {
		return nil, ErrNoRows // reuse ErrNoRows for consistency
	}
	if err != nil {
		return nil, err
	}

	var m Model
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *redisStore) ListModels(ctx context.Context) ([]*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	ids, err := s.client.SMembers(ctx, s.modelsIDsKey()).Result()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return []*Model{}, nil
	}

	// Sort numeric IDs if possible, else string sort
	sort.Slice(ids, func(i, j int) bool {
		id1, err1 := strconv.Atoi(ids[i])
		id2, err2 := strconv.Atoi(ids[j])
		if err1 == nil && err2 == nil {
			return id1 < id2
		}
		return ids[i] < ids[j]
	})

	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, s.modelsKey(id))
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	models := make([]*Model, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		strVal, ok := value.(string)
		if !ok || strVal == "" {
			continue
		}
		var m Model
		if err := json.Unmarshal([]byte(strVal), &m); err != nil {
			continue
		}
		models = append(models, &m)
	}

	return models, nil
}

// Helpers

func (s *redisStore) modelsKey(id string) string {
	return s.prefix + "models:id:" + id
}

func (s *redisStore) modelsIDsKey() string {
	return s.prefix + "models:ids"
}

func (s *redisStore) modelsNextIDKey() string {
	return s.prefix + "models:next_id"
}

func (s *redisStore) modelsModelIDMapKey() string {
	return s.prefix + "models:model_id_map"
}

func (s *redisStore) modelsChannelModelIDMapKey() string {
	return s.prefix + "models:channel_model_id_map"
}

func normalizeModelChannelKey(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return ""
	}
	channel = strings.ReplaceAll(channel, "_", "-")
	channel = strings.ReplaceAll(channel, " ", "-")
	return channel
}

func modelChannelIndexKey(channel, modelID string) string {
	return normalizeModelChannelKey(channel) + "|" + strings.TrimSpace(modelID)
}

func (s *redisStore) GetModelByModelID(ctx context.Context, modelID string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("model not found")
	}

	// Try hash index first for O(1) lookup
	id, err := s.client.HGet(ctx, s.modelsModelIDMapKey(), modelID).Result()
	if err == nil && id != "" {
		m, err := s.GetModel(ctx, id)
		if err == nil && m != nil {
			return m, nil
		}
		// Index stale, fall through to scan
	}

	// Fallback to scan (for backward compatibility with existing data)
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.ModelID == modelID {
			// Repair the index
			s.client.HSet(ctx, s.modelsModelIDMapKey(), modelID, m.ID)
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found")
}

func (s *redisStore) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, fmt.Errorf("model not found")
	}

	channelKey := modelChannelIndexKey(channel, modelID)
	id, err := s.client.HGet(ctx, s.modelsChannelModelIDMapKey(), channelKey).Result()
	if err == nil && id != "" {
		m, err := s.GetModel(ctx, id)
		if err == nil && m != nil {
			return m, nil
		}
	}

	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	wantChannel := normalizeModelChannelKey(channel)
	for _, m := range models {
		if normalizeModelChannelKey(m.Channel) == wantChannel && m.ModelID == modelID {
			s.client.HSet(ctx, s.modelsChannelModelIDMapKey(), channelKey, m.ID)
			return m, nil
		}
	}
	return nil, fmt.Errorf("model not found")
}
