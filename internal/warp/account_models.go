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
	Accounts       map[string][]string             `json:"accounts"`
	Sources        map[string]string               `json:"sources,omitempty"`
	FeatureConfigs map[string]AccountFeatureConfig `json:"feature_configs,omitempty"`
}

type AccountFeatureConfig struct {
	BaseModel             string `json:"base_model,omitempty"`
	CodingModel           string `json:"coding_model,omitempty"`
	CliAgentModel         string `json:"cli_agent_model,omitempty"`
	ComputerUseAgentModel string `json:"computer_use_agent_model,omitempty"`
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
	if len(choices.Sources) > 0 {
		normalized.Sources = make(map[string]string, len(choices.Sources))
	}
	if len(choices.FeatureConfigs) > 0 {
		normalized.FeatureConfigs = make(map[string]AccountFeatureConfig, len(choices.FeatureConfigs))
	}
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
		if normalized.Sources != nil {
			if source := strings.TrimSpace(choices.Sources[key]); source != "" {
				normalized.Sources[key] = source
			}
		}
		if normalized.FeatureConfigs != nil {
			if cfg := normalizeAccountFeatureConfig(choices.FeatureConfigs[key]); !cfg.IsEmpty() {
				normalized.FeatureConfigs[key] = cfg
			}
		}
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("marshal warp account model choices: %w", err)
	}
	return s.SetSetting(ctx, accountModelChoicesSettingKey, string(payload))
}

func SaveAccountModelChoicesForAccount(ctx context.Context, s *store.Store, accountID int64, models []string) error {
	if s == nil || accountID == 0 {
		return nil
	}
	existing, err := LoadAccountModelChoices(ctx, s)
	if err != nil {
		return err
	}
	if existing == nil {
		existing = &AccountModelChoices{Accounts: map[string][]string{}}
	}
	if existing.Accounts == nil {
		existing.Accounts = map[string][]string{}
	}
	key := strconv.FormatInt(accountID, 10)
	normalized := normalizeAccountModelIDs(models)
	if len(normalized) == 0 {
		delete(existing.Accounts, key)
		if existing.FeatureConfigs != nil {
			delete(existing.FeatureConfigs, key)
		}
	} else {
		existing.Accounts[key] = normalized
	}
	return SaveAccountModelChoices(ctx, s, existing)
}

func EffectiveAccountModelIDs(acc *store.Account, choices *AccountModelChoices) []string {
	if AccountFreeOnly(acc) {
		if choices != nil && acc != nil && acc.ID != 0 && strings.Contains(strings.TrimSpace(choices.Sources[strconv.FormatInt(acc.ID, 10)]), "free_probe") {
			if models := choices.Accounts[strconv.FormatInt(acc.ID, 10)]; len(models) > 0 {
				return models
			}
		}
		return []string{defaultModel}
	}
	if choices == nil || acc == nil || acc.ID == 0 {
		return nil
	}
	return choices.Accounts[strconv.FormatInt(acc.ID, 10)]
}

func AccountSupportsModelForAccount(choices *AccountModelChoices, acc *store.Account, modelID string) bool {
	if acc == nil || acc.ID == 0 {
		return true
	}
	modelID = canonicalModelID(modelID)
	if modelID == "" {
		return true
	}
	if choices == nil || len(choices.Accounts) == 0 {
		if !AccountFreeOnly(acc) {
			return true
		}
	}
	models := EffectiveAccountModelIDs(acc, choices)
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

func AccountFeatureConfigFromChoices(features *FeatureModelChoices) AccountFeatureConfig {
	if features == nil {
		return AccountFeatureConfig{}
	}
	return normalizeAccountFeatureConfig(AccountFeatureConfig{
		BaseModel:             features.AgentMode.DefaultID,
		CodingModel:           features.Coding.DefaultID,
		CliAgentModel:         features.CliAgent.DefaultID,
		ComputerUseAgentModel: features.ComputerUseAgent.DefaultID,
	})
}

func EffectiveAccountFeatureConfig(acc *store.Account, choices *AccountModelChoices, requestedBaseModel string) AccountFeatureConfig {
	cfg := AccountFeatureConfig{
		BaseModel:             normalizeWarpModel(requestedBaseModel),
		CliAgentModel:         identifier,
		ComputerUseAgentModel: computerUseModel,
	}
	if choices != nil && acc != nil && acc.ID != 0 {
		key := strconv.FormatInt(acc.ID, 10)
		if stored := normalizeAccountFeatureConfig(choices.FeatureConfigs[key]); !stored.IsEmpty() {
			if stored.CodingModel != "" {
				cfg.CodingModel = stored.CodingModel
			}
			if stored.CliAgentModel != "" {
				cfg.CliAgentModel = stored.CliAgentModel
			}
			if stored.ComputerUseAgentModel != "" {
				cfg.ComputerUseAgentModel = stored.ComputerUseAgentModel
			}
		}
	}
	cfg.BaseModel = normalizeWarpModel(cfg.BaseModel)
	cfg.CodingModel = canonicalModelID(cfg.CodingModel)
	cfg.CliAgentModel = firstNonEmptyModelID(cfg.CliAgentModel)
	cfg.ComputerUseAgentModel = firstNonEmptyModelID(cfg.ComputerUseAgentModel)
	if cfg.CliAgentModel == "" {
		cfg.CliAgentModel = identifier
	}
	if cfg.ComputerUseAgentModel == "" {
		cfg.ComputerUseAgentModel = computerUseModel
	}
	return cfg
}

func normalizeAccountFeatureConfig(cfg AccountFeatureConfig) AccountFeatureConfig {
	return AccountFeatureConfig{
		BaseModel:             canonicalModelID(cfg.BaseModel),
		CodingModel:           canonicalModelID(cfg.CodingModel),
		CliAgentModel:         canonicalModelID(cfg.CliAgentModel),
		ComputerUseAgentModel: canonicalModelID(cfg.ComputerUseAgentModel),
	}
}

func firstNonEmptyModelID(model string) string {
	return canonicalModelID(model)
}

func (cfg AccountFeatureConfig) IsEmpty() bool {
	return cfg.BaseModel == "" &&
		cfg.CodingModel == "" &&
		cfg.CliAgentModel == "" &&
		cfg.ComputerUseAgentModel == ""
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
