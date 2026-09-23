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
	// ContextWindows records the upstream-declared window per model id. Warp
	// publishes contextWindow{min,max,default} with every model choice, and the
	// window is a property of the model rather than of one account, so it is
	// stored once and reused. Dropping it here was what left the request builder
	// with nothing to state and made a 1M-token model look like a zero-window one.
	ContextWindows map[string]ModelContextWindow `json:"context_windows,omitempty"`
	// AccountContextWindows retains window provenance so replacing or removing
	// one account's snapshot can also remove its stale context metadata without
	// disturbing another account's last-known-good observation.
	AccountContextWindows map[string]map[string]ModelContextWindow `json:"account_context_windows,omitempty"`
}

type AccountFeatureConfig struct {
	BaseModel             string `json:"base_model,omitempty"`
	CodingModel           string `json:"coding_model,omitempty"`
	CliAgentModel         string `json:"cli_agent_model,omitempty"`
	ComputerUseAgentModel string `json:"computer_use_agent_model,omitempty"`
}

// AccountModelDiscovery is one account's verified Warp model snapshot.
// UpsertAccountModelDiscoveries is the only writer for this cache so manual
// checks, background refreshes and catalog refreshes cannot drift apart.
type AccountModelDiscovery struct {
	AccountID     int64
	Source        string
	Choices       []ModelChoice
	FeatureConfig AccountFeatureConfig
}

func UpsertAccountModelDiscoveries(ctx context.Context, s *store.Store, discoveries ...AccountModelDiscovery) error {
	if s == nil || len(discoveries) == 0 {
		return nil
	}
	existing, err := LoadAccountModelChoices(ctx, s)
	if err != nil {
		return err
	}
	if existing == nil {
		existing = &AccountModelChoices{}
	}
	if existing.Accounts == nil {
		existing.Accounts = make(map[string][]string)
	}
	if existing.Sources == nil {
		existing.Sources = make(map[string]string)
	}
	if existing.FeatureConfigs == nil {
		existing.FeatureConfigs = make(map[string]AccountFeatureConfig)
	}
	ensureAccountContextWindows(existing)
	changed := false
	for _, discovery := range discoveries {
		if discovery.AccountID == 0 || len(discovery.Choices) == 0 {
			continue
		}
		models := make([]string, 0, len(discovery.Choices))
		for _, choice := range discovery.Choices {
			if id := normalizeModelID(choice.ID); id != "" {
				models = append(models, id)
			}
		}
		models = normalizeAccountModelIDs(models)
		if len(models) == 0 {
			continue
		}
		key := strconv.FormatInt(discovery.AccountID, 10)
		existing.Accounts[key] = models
		if source := strings.TrimSpace(discovery.Source); source != "" {
			existing.Sources[key] = source
		} else {
			delete(existing.Sources, key)
		}
		if cfg := normalizeAccountFeatureConfig(discovery.FeatureConfig); !cfg.IsEmpty() {
			existing.FeatureConfigs[key] = cfg
		} else {
			delete(existing.FeatureConfigs, key)
		}
		windows := ContextWindowsFromChoices(discovery.Choices)
		if len(windows) > 0 {
			existing.AccountContextWindows[key] = windows
		} else {
			delete(existing.AccountContextWindows, key)
		}
		changed = true
	}
	if !changed {
		return nil
	}
	rebuildContextWindows(existing)
	return SaveAccountModelChoices(ctx, s, existing)
}

func RemoveAccountModelChoices(ctx context.Context, s *store.Store, accountID int64) error {
	if s == nil || accountID == 0 {
		return nil
	}
	existing, err := LoadAccountModelChoices(ctx, s)
	if err != nil || existing == nil {
		return err
	}
	key := strconv.FormatInt(accountID, 10)
	if _, ok := existing.Accounts[key]; !ok {
		return nil
	}
	ensureAccountContextWindows(existing)
	delete(existing.Accounts, key)
	delete(existing.Sources, key)
	delete(existing.FeatureConfigs, key)
	delete(existing.AccountContextWindows, key)
	rebuildContextWindows(existing)
	return SaveAccountModelChoices(ctx, s, existing)
}

// ensureAccountContextWindows upgrades caches written before context windows
// carried account provenance. It associates an observed model with every cached
// account advertising it, allowing all subsequent writes to have replacement
// semantics.
func ensureAccountContextWindows(choices *AccountModelChoices) {
	if choices == nil || choices.AccountContextWindows != nil {
		return
	}
	choices.AccountContextWindows = make(map[string]map[string]ModelContextWindow)
	for accountID, models := range choices.Accounts {
		windows := make(map[string]ModelContextWindow)
		for _, modelID := range models {
			modelID = normalizeModelID(modelID)
			if window, ok := choices.ContextWindows[modelID]; ok && window.Max > 0 {
				windows[modelID] = window
			}
		}
		if len(windows) > 0 {
			choices.AccountContextWindows[accountID] = windows
		}
	}
}

func rebuildContextWindows(choices *AccountModelChoices) {
	if choices == nil {
		return
	}
	choices.ContextWindows = nil
	for accountID, windows := range choices.AccountContextWindows {
		if _, active := choices.Accounts[accountID]; !active {
			delete(choices.AccountContextWindows, accountID)
			continue
		}
		choices.ContextWindows = MergeContextWindows(choices.ContextWindows, windows)
	}
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
	if len(choices.ContextWindows) > 0 {
		normalized.ContextWindows = make(map[string]ModelContextWindow, len(choices.ContextWindows))
		for modelID, window := range choices.ContextWindows {
			key := normalizeModelID(modelID)
			if key == "" || window.Max == 0 {
				continue
			}
			normalized.ContextWindows[key] = window
		}
	}
	if len(choices.AccountContextWindows) > 0 {
		normalized.AccountContextWindows = make(map[string]map[string]ModelContextWindow, len(choices.AccountContextWindows))
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
		if normalized.AccountContextWindows != nil {
			windows := make(map[string]ModelContextWindow)
			for modelID, window := range choices.AccountContextWindows[key] {
				modelKey := normalizeModelID(modelID)
				if modelKey != "" && window.Max > 0 {
					windows[modelKey] = window
				}
			}
			if len(windows) > 0 {
				normalized.AccountContextWindows[key] = windows
			}
		}
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("marshal warp account model choices: %w", err)
	}
	return s.SetSetting(ctx, accountModelChoicesSettingKey, string(payload))
}

// AccountSupportsModelForRouting checks the upstream-discovered model cache
// without applying the legacy free-account capability downgrade. Routing must
// present the same discovered catalog to free and paid accounts and let Warp
// decide entitlement at request time.
func AccountSupportsModelForRouting(choices *AccountModelChoices, acc *store.Account, modelID string) bool {
	if acc == nil || acc.ID == 0 || choices == nil || len(choices.Accounts) == 0 {
		return true
	}
	modelID = normalizeModelID(modelID)
	if modelID == "" {
		return true
	}
	models := choices.Accounts[strconv.FormatInt(acc.ID, 10)]
	if len(models) == 0 {
		return true
	}
	for _, model := range models {
		if normalizeModelID(model) == modelID {
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
	cfg.CodingModel = normalizeModelID(cfg.CodingModel)
	cfg.CliAgentModel = normalizeModelID(cfg.CliAgentModel)
	cfg.ComputerUseAgentModel = normalizeModelID(cfg.ComputerUseAgentModel)
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
		BaseModel:             normalizeModelID(cfg.BaseModel),
		CodingModel:           normalizeModelID(cfg.CodingModel),
		CliAgentModel:         normalizeModelID(cfg.CliAgentModel),
		ComputerUseAgentModel: normalizeModelID(cfg.ComputerUseAgentModel),
	}
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
		model = normalizeModelID(model)
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

// ContextWindowsFromChoices projects a discovery result onto the per-model
// window table. Only a real max is kept: a choice that never declared a window
// must not overwrite one another account already observed.
func ContextWindowsFromChoices(choices []ModelChoice) map[string]ModelContextWindow {
	if len(choices) == 0 {
		return nil
	}
	out := make(map[string]ModelContextWindow, len(choices))
	for _, choice := range choices {
		id := normalizeModelID(choice.ID)
		if id == "" || choice.ContextWindow.Max == 0 {
			continue
		}
		out[id] = choice.ContextWindow
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MergeContextWindows folds freshly observed windows into the stored table.
// An observation for a model not seen before is added; a model that was already
// known keeps the larger max, because a smaller report from one account must not
// shrink the window every other account can use.
func MergeContextWindows(dst map[string]ModelContextWindow, fresh map[string]ModelContextWindow) map[string]ModelContextWindow {
	if len(fresh) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]ModelContextWindow, len(fresh))
	}
	for id, window := range fresh {
		key := normalizeModelID(id)
		if key == "" || window.Max == 0 {
			continue
		}
		if existing, ok := dst[key]; ok && existing.Max >= window.Max {
			continue
		}
		dst[key] = window
	}
	return dst
}

func ContextWindowsForAccountIDs(choices *AccountModelChoices, accountIDs map[int64]struct{}) map[string]ModelContextWindow {
	if choices == nil || len(accountIDs) == 0 {
		return nil
	}
	ensureAccountContextWindows(choices)
	var out map[string]ModelContextWindow
	for accountID := range accountIDs {
		out = MergeContextWindows(out, choices.AccountContextWindows[strconv.FormatInt(accountID, 10)])
	}
	return out
}

func ModelContextWindowLimitForAccount(choices *AccountModelChoices, accountID int64, modelID string) uint32 {
	if choices == nil || accountID == 0 {
		return ModelContextWindowLimitFor(choices, modelID)
	}
	ensureAccountContextWindows(choices)
	accountChoices := &AccountModelChoices{ContextWindows: choices.AccountContextWindows[strconv.FormatInt(accountID, 10)]}
	return ModelContextWindowLimitFor(accountChoices, modelID)
}

// ModelContextWindowLimitFor resolves the input-token window to state on an
// upstream request. It answers 0 when nothing was observed, which the request
// builder renders as "field absent" so Warp keeps using the model's own max.
func ModelContextWindowLimitFor(choices *AccountModelChoices, modelID string) uint32 {
	if choices == nil || len(choices.ContextWindows) == 0 {
		return 0
	}
	normalized := normalizeModelID(modelID)
	if normalized == "" {
		return 0
	}
	if window, ok := choices.ContextWindows[normalized]; ok && window.Max > 0 {
		return window.Max
	}
	// A discovery may have published only the effort variants of a family the
	// client asked for by its bare name. Take the largest variant's window
	// rather than reporting nothing.
	largest := uint32(0)
	for id, window := range choices.ContextWindows {
		if strings.HasPrefix(id, normalized+"-") && window.Max > largest {
			largest = window.Max
		}
	}
	return largest
}
