package warp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

const accountModelChoicesSettingKey = "warp_account_model_choices"
const accountModelUnavailableSettingKey = "warp_account_model_unavailable"
const accountModelUnavailableTTL = 6 * time.Hour

type AccountModelChoices struct {
	Accounts map[string][]string `json:"accounts"`
}

type AccountModelUnavailable struct {
	Accounts map[string]map[string]string `json:"accounts"`
}

func LoadAccountModelChoices(ctx context.Context, s *store.Store) (*AccountModelChoices, error) {
	if s == nil {
		return nil, nil
	}
	raw, err := s.GetSetting(ctx, accountModelChoicesSettingKey)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var choices AccountModelChoices
	if err := json.Unmarshal([]byte(raw), &choices); err != nil {
		return nil, err
	}
	if len(choices.Accounts) == 0 {
		return nil, nil
	}
	return &choices, nil
}

func SaveAccountModelChoices(ctx context.Context, s *store.Store, choices *AccountModelChoices) error {
	if s == nil {
		return nil
	}
	if choices == nil || len(choices.Accounts) == 0 {
		return s.SetSetting(ctx, accountModelChoicesSettingKey, "")
	}
	normalized := &AccountModelChoices{Accounts: make(map[string][]string, len(choices.Accounts))}
	for accountID, models := range choices.Accounts {
		key := strings.TrimSpace(accountID)
		if key == "" {
			continue
		}
		normalizedModels := normalizeAccountModelIDs(models)
		if len(normalizedModels) == 0 {
			continue
		}
		normalized.Accounts[key] = normalizedModels
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("marshal warp account model choices: %w", err)
	}
	return s.SetSetting(ctx, accountModelChoicesSettingKey, string(payload))
}

func AccountSupportsModel(choices *AccountModelChoices, accountID int64, modelID string) bool {
	if choices == nil || len(choices.Accounts) == 0 || accountID == 0 {
		return true
	}
	modelID = canonicalModelID(modelID)
	if modelID == "" || modelID == defaultModel {
		return true
	}
	models := choices.Accounts[strconv.FormatInt(accountID, 10)]
	if len(models) == 0 {
		return true
	}
	for _, model := range models {
		if model == modelID {
			return true
		}
	}
	return false
}

func MarkAccountModelUnavailable(ctx context.Context, s *store.Store, accountID int64, modelID string, now time.Time) error {
	if s == nil || accountID == 0 {
		return nil
	}
	modelID = canonicalModelID(modelID)
	if modelID == "" || modelID == defaultModel {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	unavailable, err := loadAccountModelUnavailable(ctx, s, now)
	if err != nil {
		return err
	}
	if unavailable == nil {
		unavailable = &AccountModelUnavailable{Accounts: map[string]map[string]string{}}
	}
	accountKey := strconv.FormatInt(accountID, 10)
	models := unavailable.Accounts[accountKey]
	if models == nil {
		models = map[string]string{}
		unavailable.Accounts[accountKey] = models
	}
	models[modelID] = now.Add(accountModelUnavailableTTL).UTC().Format(time.RFC3339)
	return saveAccountModelUnavailable(ctx, s, unavailable, now)
}

func AccountModelTemporarilyUnavailable(ctx context.Context, s *store.Store, accountID int64, modelID string, now time.Time) bool {
	if s == nil || accountID == 0 {
		return false
	}
	modelID = canonicalModelID(modelID)
	if modelID == "" || modelID == defaultModel {
		return false
	}
	unavailable, err := loadAccountModelUnavailable(ctx, s, now)
	if err != nil || unavailable == nil {
		return false
	}
	expiresAt, ok := unavailable.Accounts[strconv.FormatInt(accountID, 10)][modelID]
	if !ok {
		return false
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt))
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	return now.Before(expires)
}

func FilterUnavailableModels(ctx context.Context, s *store.Store, accountID int64, choices []ModelChoice, now time.Time) []ModelChoice {
	if s == nil || accountID == 0 || len(choices) == 0 {
		return choices
	}
	out := choices[:0]
	for _, choice := range choices {
		if AccountModelTemporarilyUnavailable(ctx, s, accountID, choice.ID, now) {
			continue
		}
		out = append(out, choice)
	}
	return out
}

func loadAccountModelUnavailable(ctx context.Context, s *store.Store, now time.Time) (*AccountModelUnavailable, error) {
	if s == nil {
		return nil, nil
	}
	raw, err := s.GetSetting(ctx, accountModelUnavailableSettingKey)
	if err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var unavailable AccountModelUnavailable
	if err := json.Unmarshal([]byte(raw), &unavailable); err != nil {
		return nil, err
	}
	if len(unavailable.Accounts) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	changed := pruneExpiredAccountModelUnavailable(&unavailable, now)
	if len(unavailable.Accounts) == 0 {
		if changed {
			_ = s.SetSetting(ctx, accountModelUnavailableSettingKey, "")
		}
		return nil, nil
	}
	if changed {
		_ = saveAccountModelUnavailable(ctx, s, &unavailable, now)
	}
	return &unavailable, nil
}

func saveAccountModelUnavailable(ctx context.Context, s *store.Store, unavailable *AccountModelUnavailable, now time.Time) error {
	if s == nil {
		return nil
	}
	if unavailable == nil || len(unavailable.Accounts) == 0 {
		return s.SetSetting(ctx, accountModelUnavailableSettingKey, "")
	}
	if now.IsZero() {
		now = time.Now()
	}
	pruneExpiredAccountModelUnavailable(unavailable, now)
	if len(unavailable.Accounts) == 0 {
		return s.SetSetting(ctx, accountModelUnavailableSettingKey, "")
	}
	payload, err := json.Marshal(unavailable)
	if err != nil {
		return fmt.Errorf("marshal warp unavailable model cache: %w", err)
	}
	return s.SetSetting(ctx, accountModelUnavailableSettingKey, string(payload))
}

func pruneExpiredAccountModelUnavailable(unavailable *AccountModelUnavailable, now time.Time) bool {
	if unavailable == nil || len(unavailable.Accounts) == 0 {
		return false
	}
	changed := false
	for accountID, models := range unavailable.Accounts {
		for modelID, expiresAt := range models {
			expires, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt))
			if err != nil || !now.Before(expires) {
				delete(models, modelID)
				changed = true
			}
		}
		if len(models) == 0 {
			delete(unavailable.Accounts, accountID)
			changed = true
		}
	}
	return changed
}

func normalizeAccountModelIDs(models []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = canonicalModelID(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}
