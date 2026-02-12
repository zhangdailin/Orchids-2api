package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/util"

	"github.com/redis/go-redis/v9"
)

type redisStore struct {
	client *redis.Client
	prefix string
}

type apiKeyRecord struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"key_hash"`
	KeyFull    string     `json:"key_full,omitempty"`
	KeyPrefix  string     `json:"key_prefix"`
	KeySuffix  string     `json:"key_suffix"`
	Enabled    bool       `json:"enabled"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func newRedisStore(addr, password string, db int, prefix string) (*redisStore, error) {
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
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &redisStore{
		client: client,
		prefix: prefix,
	}, nil
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

	data, err := json.Marshal(acc)
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
	updated.SessionID = acc.SessionID
	updated.ClientCookie = acc.ClientCookie
	updated.RefreshToken = acc.RefreshToken
	if acc.SessionCookie == "" {
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
	updated.Enabled = acc.Enabled
	updated.Token = acc.Token
	updated.Subscription = acc.Subscription
	updated.UsageCurrent = acc.UsageCurrent
	updated.UsageTotal = acc.UsageTotal
	updated.UsageDaily = acc.UsageDaily
	updated.UsageLimit = acc.UsageLimit
	updated.ResetDate = acc.ResetDate
	updated.StatusCode = acc.StatusCode
	updated.LastAttempt = acc.LastAttempt
	updated.QuotaResetAt = acc.QuotaResetAt
	updated.UpdatedAt = time.Now()

	data, err := json.Marshal(&updated)
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

func (s *redisStore) IncrementRequestCount(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}

	script := redis.NewScript(`
		local key = KEYS[1]
		local now_str = ARGV[1]

		local val = redis.call("GET", key)
		if not val then return nil end

		local acc = cjson.decode(val)
		acc.request_count = (acc.request_count or 0) + 1
		acc.last_used_at = now_str
		acc.updated_at = now_str

		redis.call("SET", key, cjson.encode(acc))
		return "OK"
	`)

	nowStr := time.Now().Format(time.RFC3339Nano)
	err := script.Run(ctx, s.client, []string{s.accountsKey(id)}, nowStr).Err()
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
}

func (s *redisStore) IncrementUsage(ctx context.Context, id int64, usage float64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}
	if usage <= 0 {
		return nil
	}

	script := redis.NewScript(`
		local key = KEYS[1]
		local usage = tonumber(ARGV[1])
		local now_str = ARGV[2]

		local val = redis.call("GET", key)
		if not val then return nil end

		local acc = cjson.decode(val)
		acc.usage_current = (acc.usage_current or 0) + usage
		acc.usage_total = (acc.usage_total or 0) + usage
		acc.last_used_at = now_str
		acc.updated_at = now_str

		redis.call("SET", key, cjson.encode(acc))
		return "OK"
	`)

	nowStr := time.Now().Format(time.RFC3339Nano)
	err := script.Run(ctx, s.client, []string{s.accountsKey(id)}, usage, nowStr).Err()
	if err != nil && err != redis.Nil {
		return err
	}
	return nil
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

	script := redis.NewScript(`
		local key = KEYS[1]
		local usage = tonumber(ARGV[1])
		local count = tonumber(ARGV[2])
		local now_str = ARGV[3]
		
		local val = redis.call("GET", key)
		if not val then return redis.error_reply("account not found") end
		
		local acc = cjson.decode(val)
		
		-- Daily Reset Logic
		local today = string.sub(now_str, 1, 10)
		if acc.reset_date ~= today then
			acc.usage_daily = 0
			acc.reset_date = today
		end

		local acc_type = ""
		if acc.account_type ~= nil then
			acc_type = string.lower(tostring(acc.account_type))
		end

		-- Warp 的 usage_current 保存请求配额（由上游同步），
		-- Orchids 的 usage_current 保存 credits（由 RSC 同步）。
		-- 不能叠加 token 用量，否则会污染配额显示。
		if acc_type ~= "warp" and acc_type ~= "orchids" then
			acc.usage_current = (acc.usage_current or 0) + usage
		end
		acc.usage_total = (acc.usage_total or 0) + usage
		acc.usage_daily = (acc.usage_daily or 0) + usage
		acc.request_count = (acc.request_count or 0) + count
		acc.last_used_at = now_str
		acc.updated_at = now_str
		
		local new_val = cjson.encode(acc)
		redis.call("SET", key, new_val)
		return "OK"
	`)

	nowStr := time.Now().Format(time.RFC3339Nano)
	keys := []string{s.accountsKey(id)}
	args := []interface{}{usage, count, nowStr}

	err := script.Run(ctx, s.client, keys, args...).Err()
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

	var acc Account
	if err := json.Unmarshal([]byte(value), &acc); err != nil {
		return nil, err
	}
	if acc.ID == 0 {
		acc.ID = id
	}
	return &acc, nil
}

func (s *redisStore) getAccountsByIDs(ctx context.Context, ids []string, onlyEnabled bool) ([]*Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	idNums := make([]int64, 0, len(ids))
	for _, raw := range ids {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			idNums = append(idNums, id)
		}
	}
	if len(idNums) == 0 {
		return nil, nil
	}

	sort.Slice(idNums, func(i, j int) bool { return idNums[i] < idNums[j] })
	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.accountsKey(id))
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	// 并发阈值：少于 8 项时串行处理更高效
	const parallelThreshold = 8

	if len(values) >= parallelThreshold {
		// 并行解析 JSON
		results := make([]*Account, len(values))
		util.ParallelFor(len(values), func(idx int) {
			val := values[idx]
			if val == nil {
				return
			}
			strVal, ok := val.(string)
			if !ok || strVal == "" {
				return
			}
			var acc Account
			if err := json.Unmarshal([]byte(strVal), &acc); err != nil {
				return
			}
			if acc.ID == 0 {
				acc.ID = idNums[idx]
			}
			if onlyEnabled && !acc.Enabled {
				return
			}
			results[idx] = &acc
		})

		// 过滤 nil 结果
		accounts := make([]*Account, 0, len(values))
		for _, acc := range results {
			if acc != nil {
				accounts = append(accounts, acc)
			}
		}
		return accounts, nil
	}

	// 串行处理小批量
	accounts := make([]*Account, 0, len(values))
	for i, value := range values {
		if value == nil {
			continue
		}
		strVal, ok := value.(string)
		if !ok || strVal == "" {
			continue
		}
		var acc Account
		if err := json.Unmarshal([]byte(strVal), &acc); err != nil {
			continue
		}
		if acc.ID == 0 {
			acc.ID = idNums[i]
		}
		if onlyEnabled && !acc.Enabled {
			continue
		}
		accounts = append(accounts, &acc)
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

func (s *redisStore) GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("redis store not configured")
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, nil
	}
	idStr, err := s.client.Get(ctx, s.apiKeysHashKey(hash)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id == 0 {
		return nil, nil
	}
	return s.getApiKeyByID(ctx, id)
}

func (s *redisStore) UpdateApiKeyEnabled(ctx context.Context, id int64, enabled bool) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err == ErrNoRows {
		return ErrNoRows
	}
	if err != nil {
		return err
	}
	key.Enabled = enabled
	record := apiKeyRecordFromKey(key)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := s.client.Set(ctx, s.apiKeysKey(id), data, 0).Err(); err != nil {
		return err
	}
	return nil
}

func (s *redisStore) UpdateApiKeyLastUsed(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return nil
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err == ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now()
	key.LastUsedAt = &now
	record := apiKeyRecordFromKey(key)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.apiKeysKey(id), data, 0).Err()
}

func (s *redisStore) DeleteApiKey(ctx context.Context, id int64) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("redis store not configured")
	}
	if id == 0 {
		return ErrNoRows
	}
	key, err := s.getApiKeyByID(ctx, id)
	if err == ErrNoRows {
		return ErrNoRows
	}
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

	idNums := make([]int64, 0, len(ids))
	for _, raw := range ids {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			idNums = append(idNums, id)
		}
	}
	if len(idNums) == 0 {
		return nil, nil
	}

	sort.Slice(idNums, func(i, j int) bool { return idNums[i] < idNums[j] })
	keys := make([]string, 0, len(idNums))
	for _, id := range idNums {
		keys = append(keys, s.apiKeysKey(id))
	}

	values, err := s.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	const parallelThreshold = 8

	if len(values) >= parallelThreshold {
		results := make([]*ApiKey, len(values))
		util.ParallelFor(len(values), func(idx int) {
			val := values[idx]
			if val == nil {
				return
			}
			strVal, ok := val.(string)
			if !ok || strVal == "" {
				return
			}
			var record apiKeyRecord
			if err := json.Unmarshal([]byte(strVal), &record); err != nil {
				return
			}
			key := record.toApiKey()
			if key.ID == 0 {
				key.ID = idNums[idx]
			}
			results[idx] = key
		})

		items := make([]*ApiKey, 0, len(values))
		for _, key := range results {
			if key != nil {
				items = append(items, key)
			}
		}
		return items, nil
	}

	items := make([]*ApiKey, 0, len(values))
	for i, value := range values {
		if value == nil {
			continue
		}
		strVal, ok := value.(string)
		if !ok || strVal == "" {
			continue
		}
		var record apiKeyRecord
		if err := json.Unmarshal([]byte(strVal), &record); err != nil {
			continue
		}
		key := record.toApiKey()
		if key.ID == 0 {
			key.ID = idNums[i]
		}
		items = append(items, key)
	}

	return items, nil
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

func apiKeyRecordFromKey(key *ApiKey) apiKeyRecord {
	if key == nil {
		return apiKeyRecord{}
	}
	return apiKeyRecord{
		ID:         key.ID,
		Name:       key.Name,
		KeyHash:    key.KeyHash,
		KeyFull:    key.KeyFull,
		KeyPrefix:  key.KeyPrefix,
		KeySuffix:  key.KeySuffix,
		Enabled:    key.Enabled,
		LastUsedAt: key.LastUsedAt,
		CreatedAt:  key.CreatedAt,
	}
}

func (r apiKeyRecord) toApiKey() *ApiKey {
	return &ApiKey{
		ID:         r.ID,
		Name:       r.Name,
		KeyHash:    r.KeyHash,
		KeyFull:    r.KeyFull,
		KeyPrefix:  r.KeyPrefix,
		KeySuffix:  r.KeySuffix,
		Enabled:    r.Enabled,
		LastUsedAt: r.LastUsedAt,
		CreatedAt:  r.CreatedAt,
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

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.Set(ctx, s.modelsKey(m.ID), data, 0)
	pipe.SAdd(ctx, s.modelsIDsKey(), m.ID)
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

	pipe := s.client.Pipeline()
	pipe.Del(ctx, s.modelsKey(id))
	pipe.SRem(ctx, s.modelsIDsKey(), id)
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
