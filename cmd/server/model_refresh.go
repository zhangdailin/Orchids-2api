package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

type modelRefreshRequest struct {
	Channel string `json:"channel"`
}

type modelRefreshResult struct {
	Channel         string   `json:"channel"`
	Source          string   `json:"source"`
	Discovered      int      `json:"discovered"`
	Verified        int      `json:"verified"`
	Added           int      `json:"added"`
	Updated         int      `json:"updated"`
	Deleted         int      `json:"deleted"`
	DefaultModelID  string   `json:"default_model_id,omitempty"`
	AddedModelIDs   []string `json:"added_model_ids,omitempty"`
	DeletedModelIDs []string `json:"deleted_model_ids,omitempty"`
}

type discoveredModel struct {
	ID        string
	Name      string
	SortOrder int
}

var runModelRefresh = syncModelsForChannel

func makeModelRefreshHandler(cfg *config.Config, s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channel := strings.TrimSpace(r.URL.Query().Get("channel"))
		if r.Body != nil {
			defer r.Body.Close()
			var req modelRefreshRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil && strings.TrimSpace(req.Channel) != "" {
				channel = strings.TrimSpace(req.Channel)
			}
		}

		result, err := runModelRefresh(r.Context(), cfg, s, channel)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func syncModelsForChannel(ctx context.Context, cfg *config.Config, s *store.Store, channel string) (*modelRefreshResult, error) {
	channel = normalizeAdminModelChannel(channel)
	if channel == "" {
		return nil, fmt.Errorf("channel is required")
	}
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}

	candidates, source, err := discoverModelsForChannel(ctx, cfg, s, channel)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s has no discoverable models", channel)
	}

	return applyModelRefresh(ctx, s, channel, source, candidates)
}

func normalizeAdminModelChannel(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "orchids":
		return "Orchids"
	case "warp":
		return "Warp"
	case "bolt":
		return "Bolt"
	case "puter":
		return "Puter"
	case "grok":
		return "Grok"
	default:
		return ""
	}
}

func discoverModelsForChannel(ctx context.Context, cfg *config.Config, s *store.Store, channel string) ([]discoveredModel, string, error) {
	switch strings.ToLower(channel) {
	case "orchids":
		items, source, err := fetchOrchidsModelChoices(ctx, cfg, s)
		if err != nil && len(items) == 0 {
			return nil, "", err
		}
		out := make([]discoveredModel, 0, len(items))
		for i, item := range items {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = id
			}
			out = append(out, discoveredModel{ID: id, Name: name, SortOrder: i})
		}
		return out, source, nil
	case "warp":
		return discoverWarpModels(ctx, cfg, s)
	case "bolt":
		items := store.BuildBoltSeedModels(ctx)
		out := make([]discoveredModel, 0, len(items))
		for i, item := range items {
			out = append(out, discoveredModel{
				ID:        strings.TrimSpace(item.ModelID),
				Name:      strings.TrimSpace(item.Name),
				SortOrder: i,
			})
		}
		return out, "bolt_bundle", nil
	case "puter":
		return discoverPuterModels(ctx, cfg, s)
	case "grok":
		return discoverGrokModels(ctx, cfg, s)
	default:
		return nil, "", fmt.Errorf("unsupported channel: %s", channel)
	}
}

func discoverPuterModels(ctx context.Context, cfg *config.Config, s *store.Store) ([]discoveredModel, string, error) {
	proxyFunc := http.ProxyFromEnvironment
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
	}
	items, err := fetchPuterPublicModelChoices(ctx, proxyFunc)
	source := "puter_public_models"
	if err != nil || len(items) == 0 {
		source = "puter_static_catalog_fallback"
		items = puterPublicChoicesFromDiscovered(puterSeedDiscoveredModels())
	}

	candidates := puterChoicesToDiscovered(items)
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("puter has no discoverable models")
	}

	accounts, accErr := enabledAccountsByType(ctx, s, "puter")
	if accErr != nil || len(accounts) == 0 {
		if accErr != nil {
			return candidates, source + "_unverified", nil
		}
		return candidates, source + "_unverified", nil
	}

	verified := verifyPuterDiscoveredModels(ctx, cfg, accounts, candidates)
	if len(verified) == 0 {
		return nil, "", fmt.Errorf("no puter models verified by test_mode")
	}
	return verified, source + "_test_mode", nil
}

func puterChoicesToDiscovered(items []puterPublicModelChoice) []discoveredModel {
	out := make([]discoveredModel, 0, len(items))
	for i, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: i})
	}
	return out
}

func puterPublicChoicesFromDiscovered(items []discoveredModel) []puterPublicModelChoice {
	out := make([]puterPublicModelChoice, 0, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		out = append(out, puterPublicModelChoice{ID: id, Name: name})
	}
	return out
}

func verifyPuterDiscoveredModels(ctx context.Context, cfg *config.Config, accounts []*store.Account, candidates []discoveredModel) []discoveredModel {
	if len(accounts) == 0 || len(candidates) == 0 {
		return nil
	}
	verified := make([]discoveredModel, 0, len(candidates))
	accountIndex := 0
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		ok := false
		for attempt := 0; attempt < len(accounts); attempt++ {
			acc := accounts[(accountIndex+attempt)%len(accounts)]
			client := puter.NewFromAccount(acc, refreshModelRequestConfig(cfg, "puter"))
			err := client.VerifyModel(ctx, candidate.ID)
			client.Close()
			if err == nil {
				ok = true
				accountIndex = (accountIndex + attempt + 1) % len(accounts)
				break
			}
		}
		if ok {
			candidate.SortOrder = len(verified)
			verified = append(verified, candidate)
		}
	}
	return verified
}

func puterSeedDiscoveredModels() []discoveredModel {
	items := puter.SeedModels(0)
	out := make([]discoveredModel, 0, len(items))
	for i, item := range items {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{
			ID:        id,
			Name:      name,
			SortOrder: i,
		})
	}
	return out
}

func discoverGrokModels(ctx context.Context, cfg *config.Config, s *store.Store) ([]discoveredModel, string, error) {
	accounts, err := enabledAccountsByType(ctx, s, "grok")
	if err != nil {
		return nil, "", err
	}
	if len(accounts) == 0 {
		return nil, "", fmt.Errorf("no enabled grok accounts")
	}

	token := firstGrokToken(accounts)
	if token == "" {
		return nil, "", fmt.Errorf("no enabled grok account token")
	}

	candidates := grokProbeCandidateModels(ctx, s)
	client := grok.New(refreshModelRequestConfig(cfg, "grok"))
	out := make([]discoveredModel, 0, len(candidates))
	for _, candidate := range candidates {
		if spec, ok := grok.ResolveModel(candidate.ID); ok && (spec.IsImage || spec.IsVideo) {
			out = append(out, discoveredModel{
				ID:        candidate.ID,
				Name:      candidate.Name,
				SortOrder: len(out),
			})
			continue
		}
		result := client.ProbeConsoleModel(ctx, token, candidate.ID)
		if !result.OK {
			continue
		}
		if !isAcceptedGrokCanonical(candidate.ID, result.CanonicalModel) {
			continue
		}
		out = append(out, discoveredModel{
			ID:        candidate.ID,
			Name:      candidate.Name,
			SortOrder: len(out),
		})
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("no grok models verified by console.x.ai")
	}
	return out, "grok_console_probe", nil
}

func firstGrokToken(accounts []*store.Account) string {
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		token := grok.NormalizeSSOToken(firstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
		if token != "" {
			return token
		}
	}
	return ""
}

func grokProbeCandidateModels(ctx context.Context, s *store.Store) []discoveredModel {
	seen := map[string]struct{}{}
	out := make([]discoveredModel, 0, 16)
	appendCandidate := func(id, name string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		if strings.TrimSpace(name) == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: len(out)})
	}

	for _, id := range modelpolicy.PublicGrokModelIDs() {
		name := id
		if spec, ok := grok.ResolveModel(id); ok && strings.TrimSpace(spec.Name) != "" {
			name = spec.Name
		}
		appendCandidate(id, name)
	}

	for _, id := range []string{
		"grok-4.3",
		"grok-4.3-latest",
		"grok-latest",
		"grok-4.20",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-0309-reasoning",
		"grok-420",
		"grok-3-mini",
		"grok-4-thinking",
		"grok-4.1-expert",
	} {
		name := id
		if spec, ok := grok.ResolveModel(id); ok && strings.TrimSpace(spec.Name) != "" {
			name = spec.Name
		}
		appendCandidate(id, name)
	}

	if s != nil {
		if existing, err := s.ListModels(ctx); err == nil {
			for _, model := range existing {
				if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "grok") {
					continue
				}
				appendCandidate(model.ModelID, model.Name)
			}
		}
	}

	return out
}

func isAcceptedGrokCanonical(requested, canonical string) bool {
	requested = strings.ToLower(strings.TrimSpace(requested))
	canonical = strings.ToLower(strings.TrimSpace(canonical))
	if requested == "" || canonical == "" {
		return false
	}
	if requested == canonical {
		return true
	}
	switch requested {
	case "grok-4.3-latest", "grok-latest":
		return canonical == "grok-4.3"
	default:
		return false
	}
}

func discoverWarpModels(ctx context.Context, cfg *config.Config, s *store.Store) ([]discoveredModel, string, error) {
	if s == nil {
		return warpSeedDiscoveredModels(), "warp_static_catalog", nil
	}

	accounts, err := enabledAccountsByType(ctx, s, "warp")
	if err != nil {
		return nil, "", err
	}

	seen := map[string]struct{}{}
	out := make([]discoveredModel, 0, 24)
	sourceSet := map[string]struct{}{}
	appendChoice := func(choice warp.ModelChoice) {
		id := strings.TrimSpace(choice.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		name := firstNonEmpty(choice.Name, id)
		out = append(out, discoveredModel{
			ID:        id,
			Name:      name,
			SortOrder: len(out),
		})
	}

	for _, acc := range accounts {
		client := warp.NewFromAccount(acc, cfg)
		choices, source, discoverErr := client.FetchDiscoveredModelChoices(ctx)
		client.Close()
		if discoverErr != nil {
			continue
		}
		for _, part := range strings.Split(source, "+") {
			part = strings.TrimSpace(part)
			if part != "" {
				sourceSet[part] = struct{}{}
			}
		}
		for _, choice := range choices {
			appendChoice(choice)
		}
	}

	if len(out) > 0 {
		return out, joinWarpDiscoverySources(sourceSet), nil
	}
	return warpSeedDiscoveredModels(), "warp_static_catalog_fallback", nil
}

func warpSeedDiscoveredModels() []discoveredModel {
	items := store.BuildWarpSeedModels()
	out := make([]discoveredModel, 0, len(items))
	for i, item := range items {
		out = append(out, discoveredModel{
			ID:        strings.TrimSpace(item.ModelID),
			Name:      strings.TrimSpace(item.Name),
			SortOrder: i,
		})
	}
	return out
}

func joinWarpDiscoverySources(sourceSet map[string]struct{}) string {
	if len(sourceSet) == 0 {
		return "warp_graphql"
	}
	ordered := make([]string, 0, 2)
	for _, part := range []string{"agent_mode_llms", "workspace_available_llms"} {
		if _, ok := sourceSet[part]; ok {
			ordered = append(ordered, part)
		}
	}
	if len(ordered) == 0 {
		for part := range sourceSet {
			ordered = append(ordered, part)
		}
		sort.Strings(ordered)
	}
	return "warp_graphql_" + strings.Join(ordered, "+")
}

func refreshModelRequestConfig(cfg *config.Config, channel string) *config.Config {
	if cfg == nil {
		cfg = &config.Config{}
	} else {
		copyCfg := *cfg
		cfg = &copyCfg
	}

	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "orchids":
		if cfg.RequestTimeout <= 0 || cfg.RequestTimeout > 10 {
			cfg.RequestTimeout = 10
		}
	case "warp", "bolt", "puter":
		if cfg.RequestTimeout <= 0 || cfg.RequestTimeout > 15 {
			cfg.RequestTimeout = 15
		}
	}

	return cfg
}

func enabledAccountsByType(ctx context.Context, s *store.Store, accountType string) ([]*store.Account, error) {
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), accountType) {
			out = append(out, acc)
		}
	}
	return out, nil
}

func applyModelRefresh(ctx context.Context, s *store.Store, channel string, source string, candidates []discoveredModel) (*modelRefreshResult, error) {
	existingModels, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	result := &modelRefreshResult{
		Channel:    channel,
		Source:     source,
		Discovered: len(candidates),
	}

	existingByID := make(map[string]*store.Model)
	fetchedSet := make(map[string]discoveredModel, len(candidates))
	for _, model := range candidates {
		fetchedSet[model.ID] = model
	}
	result.Verified = len(candidates)

	for _, model := range existingModels {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), channel) {
			continue
		}
		existingByID[model.ModelID] = model
	}

	defaultModelID := chooseRefreshedDefaultModel(existingByID, candidates)
	result.DefaultModelID = defaultModelID

	for _, model := range candidates {
		existing := existingByID[model.ID]
		if existing == nil {
			record := &store.Model{
				Channel:   channel,
				ModelID:   model.ID,
				Name:      firstNonEmpty(model.Name, model.ID),
				Status:    store.ModelStatusAvailable,
				Verified:  true,
				IsDefault: model.ID == defaultModelID,
				SortOrder: model.SortOrder,
			}
			if err := s.CreateModel(ctx, record); err != nil {
				return nil, err
			}
			result.Added++
			result.AddedModelIDs = append(result.AddedModelIDs, model.ID)
			continue
		}

		needsUpdate := false
		if !strings.EqualFold(existing.Channel, channel) {
			existing.Channel = channel
			needsUpdate = true
		}
		if existing.Name != firstNonEmpty(model.Name, model.ID) {
			existing.Name = firstNonEmpty(model.Name, model.ID)
			needsUpdate = true
		}
		if existing.SortOrder != model.SortOrder {
			existing.SortOrder = model.SortOrder
			needsUpdate = true
		}
		if existing.Status != store.ModelStatusAvailable {
			existing.Status = store.ModelStatusAvailable
			needsUpdate = true
		}
		if !existing.Verified {
			existing.Verified = true
			needsUpdate = true
		}
		desiredDefault := model.ID == defaultModelID
		if existing.IsDefault != desiredDefault {
			existing.IsDefault = desiredDefault
			needsUpdate = true
		}
		if needsUpdate {
			if err := s.UpdateModel(ctx, existing); err != nil {
				return nil, err
			}
			result.Updated++
		}
	}

	for modelID, existing := range existingByID {
		if _, ok := fetchedSet[modelID]; ok {
			continue
		}
		if err := s.DeleteModel(ctx, existing.ID); err != nil {
			return nil, err
		}
		result.Deleted++
		result.DeletedModelIDs = append(result.DeletedModelIDs, modelID)
	}

	sort.Strings(result.AddedModelIDs)
	sort.Strings(result.DeletedModelIDs)
	return result, nil
}

func chooseRefreshedDefaultModel(existing map[string]*store.Model, ordered []discoveredModel) string {
	for _, model := range ordered {
		if current := existing[model.ID]; current != nil && current.IsDefault {
			return model.ID
		}
	}
	for _, model := range ordered {
		return model.ID
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
