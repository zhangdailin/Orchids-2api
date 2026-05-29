package warp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

const accountModelChoicesSettingKey = "warp_account_model_choices"

type AccountModelChoices struct {
	Accounts map[string][]string `json:"accounts"`
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
