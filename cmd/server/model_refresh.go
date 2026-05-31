package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/puter"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
)

const (
	defaultModelRefreshConcurrency = 4
	maxModelRefreshConcurrency     = 16
)

var verifyPuterModelForRefresh = func(ctx context.Context, cfg *config.Config, acc *store.Account, modelID string) error {
	client := puter.NewFromAccount(acc, refreshModelRequestConfig(cfg, "puter"))
	defer client.Close()
	return client.VerifyModel(ctx, modelID)
}

var probeWarpModelForRefresh = func(ctx context.Context, cfg *config.Config, acc *store.Account, modelID string) error {
	client := warp.NewFromAccount(acc, refreshModelRequestConfig(cfg, "warp"))
	defer client.Close()
	return client.ProbeModel(ctx, modelID)
}

type modelRefreshRequest struct {
	Channel     string `json:"channel"`
	Concurrency int    `json:"concurrency,omitempty"`
}

type modelRefreshResult struct {
	Channel         string   `json:"channel"`
	Source          string   `json:"source"`
	Concurrency     int      `json:"concurrency"`
	Discovered      int      `json:"discovered"`
	Verified        int      `json:"verified"`
	Added           int      `json:"added"`
	Updated         int      `json:"updated"`
	Deleted         int      `json:"deleted"`
	Offline         int      `json:"offline"`
	DefaultModelID  string   `json:"default_model_id,omitempty"`
	AddedModelIDs   []string `json:"added_model_ids,omitempty"`
	DeletedModelIDs []string `json:"deleted_model_ids,omitempty"`
	OfflineModelIDs []string `json:"offline_model_ids,omitempty"`
}

type discoveredModel struct {
	ID        string
	Name      string
	SortOrder int
}

type warpAccountDiscovery struct {
	index   int
	id      int64
	choices []warp.ModelChoice
	source  string
	ok      bool
}

type modelRefreshFunc func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error)

var runModelRefresh modelRefreshFunc = syncModelsForChannelConcurrent

func makeModelRefreshHandler(cfg *config.Config, s *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channel := strings.TrimSpace(r.URL.Query().Get("channel"))
		concurrency := defaultModelRefreshConcurrency
		if parsed, ok := parseModelRefreshConcurrency(r.URL.Query().Get("concurrency")); ok {
			concurrency = parsed
		}
		if r.Body != nil {
			defer r.Body.Close()
			var req modelRefreshRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil && strings.TrimSpace(req.Channel) != "" {
				channel = strings.TrimSpace(req.Channel)
			}
			if req.Concurrency != 0 {
				concurrency = normalizeModelRefreshConcurrency(req.Concurrency)
			}
		}

		result, err := runModelRefresh(r.Context(), cfg, s, channel, concurrency)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if result != nil && result.Concurrency == 0 {
			result.Concurrency = concurrency
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func syncModelsForChannel(ctx context.Context, cfg *config.Config, s *store.Store, channel string) (*modelRefreshResult, error) {
	return syncModelsForChannelConcurrent(ctx, cfg, s, channel, defaultModelRefreshConcurrency)
}

func syncModelsForChannelConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error) {
	channel = normalizeAdminModelChannel(channel)
	if channel == "" {
		return nil, fmt.Errorf("channel is required")
	}
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}

	concurrency = normalizeModelRefreshConcurrency(concurrency)
	candidates, source, err := discoverModelsForChannelConcurrent(ctx, cfg, s, channel, concurrency)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s has no discoverable models", channel)
	}

	result, err := applyModelRefresh(ctx, s, channel, source, candidates)
	if result != nil {
		result.Concurrency = concurrency
	}
	return result, err
}

func normalizeAdminModelChannel(channel string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "orchids":
		return "Orchids"
	case "warp":
		return "Warp"
	case "puter":
		return "Puter"
	case "grok":
		return "Grok"
	default:
		return ""
	}
}

func parseModelRefreshConcurrency(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return defaultModelRefreshConcurrency, true
	}
	return normalizeModelRefreshConcurrency(value), true
}

func normalizeModelRefreshConcurrency(concurrency int) int {
	if concurrency <= 0 {
		return defaultModelRefreshConcurrency
	}
	if concurrency > maxModelRefreshConcurrency {
		return maxModelRefreshConcurrency
	}
	return concurrency
}

func boundedModelRefreshWorkers(total int, concurrency int) int {
	if total <= 0 {
		return 0
	}
	workers := normalizeModelRefreshConcurrency(concurrency)
	if workers > total {
		workers = total
	}
	return workers
}

func discoverModelsForChannel(ctx context.Context, cfg *config.Config, s *store.Store, channel string) ([]discoveredModel, string, error) {
	return discoverModelsForChannelConcurrent(ctx, cfg, s, channel, defaultModelRefreshConcurrency)
}

func discoverModelsForChannelConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) ([]discoveredModel, string, error) {
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
		return discoverWarpModelsConcurrent(ctx, cfg, s, concurrency)
	case "puter":
		return discoverPuterModelsConcurrent(ctx, cfg, s, concurrency)
	case "grok":
		return discoverGrokModelsConcurrent(ctx, cfg, s, concurrency)
	default:
		return nil, "", fmt.Errorf("unsupported channel: %s", channel)
	}
}

func discoverPuterModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
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

	verified := verifyPuterDiscoveredModelsConcurrent(ctx, cfg, accounts, candidates, concurrency)
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

func verifyPuterDiscoveredModelsConcurrent(ctx context.Context, cfg *config.Config, accounts []*store.Account, candidates []discoveredModel, concurrency int) []discoveredModel {
	if len(accounts) == 0 || len(candidates) == 0 {
		return nil
	}
	workerCount := boundedModelRefreshWorkers(len(candidates), concurrency)
	if workerCount <= 1 {
		return verifyPuterDiscoveredModelsSerial(ctx, cfg, accounts, candidates)
	}

	results := make([]puterModelProbeResult, len(candidates))
	jobs := make(chan int, len(candidates))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				candidate := candidates[idx]
				if strings.TrimSpace(candidate.ID) == "" {
					continue
				}
				startAccount := idx % len(accounts)
				allDefinitiveRejects := true
				for attempt := 0; attempt < len(accounts); attempt++ {
					if err := ctx.Err(); err != nil {
						allDefinitiveRejects = false
						break
					}
					acc := accounts[(startAccount+attempt)%len(accounts)]
					err := verifyPuterModelForRefresh(ctx, cfg, acc, candidate.ID)
					if err == nil {
						results[idx] = puterModelProbeAccepted
						break
					}
					if !isPuterModelDefinitiveReject(err) {
						allDefinitiveRejects = false
					}
				}
				if results[idx] != puterModelProbeAccepted && allDefinitiveRejects {
					results[idx] = puterModelProbeRejected
				}
			}
		}()
	}
	for idx := range candidates {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()

	verified := make([]discoveredModel, 0, len(candidates))
	for idx, candidate := range candidates {
		if results[idx] != puterModelProbeAccepted {
			continue
		}
		candidate.SortOrder = len(verified)
		verified = append(verified, candidate)
	}
	return verified
}

func verifyPuterDiscoveredModelsSerial(ctx context.Context, cfg *config.Config, accounts []*store.Account, candidates []discoveredModel) []discoveredModel {
	verified := make([]discoveredModel, 0, len(candidates))
	accountIndex := 0
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		ok := false
		for attempt := 0; attempt < len(accounts); attempt++ {
			if err := ctx.Err(); err != nil {
				break
			}
			acc := accounts[(accountIndex+attempt)%len(accounts)]
			err := verifyPuterModelForRefresh(ctx, cfg, acc, candidate.ID)
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

type puterModelProbeResult uint8

const (
	puterModelProbeUnknown puterModelProbeResult = iota
	puterModelProbeAccepted
	puterModelProbeRejected
)

func isPuterModelDefinitiveReject(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	if strings.Contains(text, "model not found") ||
		strings.Contains(text, "invalid model") ||
		strings.Contains(text, "unknown model") ||
		strings.Contains(text, "unsupported model") {
		return true
	}
	return false
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

func discoverGrokModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	accounts, err := enabledAccountsByType(ctx, s, "grok")
	if err != nil {
		return nil, "", err
	}
	if len(accounts) == 0 {
		return nil, "", fmt.Errorf("no enabled grok accounts")
	}

	tokens := grokAccountTokens(accounts)
	if len(tokens) == 0 {
		return nil, "", fmt.Errorf("no enabled grok account token")
	}

	candidates := canonicalizeDiscoveredModels(grokProbeCandidateModels(ctx, s), canonicalGrokRefreshModelID)
	client := grok.New(refreshModelRequestConfig(cfg, "grok"))
	accepted := make([]bool, len(candidates))
	workerCount := boundedModelRefreshWorkers(len(candidates), concurrency)
	if workerCount <= 1 {
		token := tokens[0]
		for i, candidate := range candidates {
			if spec, ok := grok.ResolveModel(candidate.ID); ok && (spec.IsImage || spec.IsVideo) {
				accepted[i] = true
				continue
			}
			if spec, ok := grok.ResolveModel(candidate.ID); ok && strings.TrimSpace(spec.ConsoleModel) != "" {
				accepted[i] = true
				continue
			}
			result := client.ProbeConsoleModel(ctx, token, candidate.ID)
			accepted[i] = result.OK && isAcceptedGrokCanonical(candidate.ID, result.CanonicalModel)
		}
	} else {
		jobs := make(chan int, len(candidates))
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go func() {
				defer wg.Done()
				for idx := range jobs {
					candidate := candidates[idx]
					if strings.TrimSpace(candidate.ID) == "" {
						continue
					}
					if spec, ok := grok.ResolveModel(candidate.ID); ok && (spec.IsImage || spec.IsVideo) {
						accepted[idx] = true
						continue
					}
					if spec, ok := grok.ResolveModel(candidate.ID); ok && strings.TrimSpace(spec.ConsoleModel) != "" {
						accepted[idx] = true
						continue
					}
					if err := ctx.Err(); err != nil {
						continue
					}
					token := tokens[idx%len(tokens)]
					result := client.ProbeConsoleModel(ctx, token, candidate.ID)
					accepted[idx] = result.OK && isAcceptedGrokCanonical(candidate.ID, result.CanonicalModel)
				}
			}()
		}
		for idx := range candidates {
			jobs <- idx
		}
		close(jobs)
		wg.Wait()
	}

	for idx, candidate := range candidates {
		if accepted[idx] {
			continue
		}
		if spec, ok := grok.ResolveModel(candidate.ID); ok && strings.TrimSpace(spec.ConsoleModel) != "" {
			accepted[idx] = true
		}
	}

	out := make([]discoveredModel, 0, len(candidates))
	for idx, candidate := range candidates {
		if !accepted[idx] {
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

func canonicalGrokRefreshModelID(modelID string) string {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return ""
	}
	if spec, ok := grok.ResolveModel(id); ok {
		return spec.ID
	}
	return id
}

func canonicalizeDiscoveredModels(items []discoveredModel, normalize func(string) string) []discoveredModel {
	seen := map[string]struct{}{}
	out := make([]discoveredModel, 0, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if normalize != nil {
			id = strings.TrimSpace(normalize(id))
		}
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(item.Name)
		if spec, ok := grok.ResolveModel(id); ok && strings.TrimSpace(spec.Name) != "" {
			name = strings.TrimSpace(spec.Name)
		}
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: len(out)})
	}
	return out
}

func grokAccountTokens(accounts []*store.Account) []string {
	seen := map[string]struct{}{}
	tokens := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		token := grok.NormalizeSSOToken(firstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
		if token == "" {
			continue
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
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
		"grok-4.20-0309",
		"grok-4.20-0309-non-reasoning",
		"grok-4.20-0309-reasoning",
		"grok-4.20-fast",
		"grok-4.20-auto",
		"grok-4.20-expert",
		"grok-4.20-heavy",
		"grok-4.3",
		"grok-build-0.1",
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
	return false
}

func discoverWarpModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
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

	workerCount := boundedModelRefreshWorkers(len(accounts), concurrency)
	if workerCount > 0 {
		jobs := make(chan int, len(accounts))
		results := make(chan warpAccountDiscovery, len(accounts))
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go func() {
				defer wg.Done()
				for idx := range jobs {
					acc := accounts[idx]
					client := warp.NewFromAccount(acc, cfg)
					choices, source, discoverErr := client.FetchDiscoveredModelChoices(ctx)
					client.Close()
					if discoverErr != nil {
						continue
					}
					choices = warp.FilterUnavailableModels(ctx, s, acc.ID, choices, time.Now())
					if warp.AccountFreeOnly(acc) {
						choices, source = probeWarpFreeOnlyModelChoices(ctx, cfg, acc, choices)
						if len(choices) == 0 {
							choices = []warp.ModelChoice{{ID: warp.DefaultModel(), Name: "Warp Auto Open"}}
							source = "free_probe_fallback"
						}
					}
					results <- warpAccountDiscovery{
						index:   idx,
						id:      acc.ID,
						choices: choices,
						source:  source,
						ok:      true,
					}
				}
			}()
		}
		for idx := range accounts {
			jobs <- idx
		}
		close(jobs)
		wg.Wait()
		close(results)

		ordered := make([]warpAccountDiscovery, len(accounts))
		for result := range results {
			if result.index >= 0 && result.index < len(ordered) {
				ordered[result.index] = result
			}
		}
		for _, result := range ordered {
			if !result.ok {
				continue
			}
			for _, part := range strings.Split(result.source, "+") {
				part = strings.TrimSpace(part)
				if part != "" {
					sourceSet[part] = struct{}{}
				}
			}
			for _, choice := range result.choices {
				appendChoice(choice)
			}
		}
		if len(out) > 0 {
			saveWarpAccountModelChoices(ctx, s, ordered)
		}
	}

	if len(out) > 0 {
		return out, joinWarpDiscoverySources(sourceSet), nil
	}
	return warpSeedDiscoveredModels(), "warp_static_catalog_fallback", nil
}

func probeWarpFreeOnlyModelChoices(ctx context.Context, cfg *config.Config, acc *store.Account, discovered []warp.ModelChoice) ([]warp.ModelChoice, string) {
	candidates := warpFreeOnlyProbeCandidates(discovered)
	out := make([]warp.ModelChoice, 0, len(candidates))
	for _, choice := range candidates {
		if err := probeWarpModelForRefresh(ctx, cfg, acc, choice.ID); err != nil {
			continue
		}
		out = append(out, choice)
	}
	if len(out) == 0 {
		return nil, "free_probe"
	}
	return out, "free_probe"
}

func warpFreeOnlyProbeCandidates(discovered []warp.ModelChoice) []warp.ModelChoice {
	preferred := []string{
		warp.DefaultModel(),
		"claude-4-5-haiku",
		"claude-4-5-sonnet",
		"claude-4-5-opus",
		"gpt-5-2-low",
		"gpt-5-1-low",
		"gemini-3-5-flash",
	}
	byID := make(map[string]warp.ModelChoice, len(discovered)+len(preferred))
	for _, choice := range discovered {
		id := warp.NormalizeModelID(choice.ID)
		if id == "" {
			continue
		}
		choice.ID = id
		if strings.TrimSpace(choice.Name) == "" {
			choice.Name = id
		}
		byID[id] = choice
	}
	out := make([]warp.ModelChoice, 0, len(preferred))
	seen := map[string]struct{}{}
	for _, id := range preferred {
		id = warp.NormalizeModelID(id)
		if id == "" {
			continue
		}
		choice, ok := byID[id]
		if !ok && id == warp.DefaultModel() {
			choice = warp.ModelChoice{ID: id, Name: "Warp Auto Open"}
			ok = true
		}
		if !ok {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, choice)
	}
	return out
}

func saveWarpAccountModelChoices(ctx context.Context, s *store.Store, discoveries []warpAccountDiscovery) {
	if s == nil {
		return
	}
	accountChoices := &warp.AccountModelChoices{Accounts: make(map[string][]string)}
	accountChoices.Sources = make(map[string]string)
	for _, result := range discoveries {
		if !result.ok || result.id == 0 || len(result.choices) == 0 {
			continue
		}
		models := make([]string, 0, len(result.choices))
		for _, choice := range result.choices {
			models = append(models, choice.ID)
		}
		key := strconv.FormatInt(result.id, 10)
		accountChoices.Accounts[key] = models
		if source := strings.TrimSpace(result.source); source != "" {
			accountChoices.Sources[key] = source
		}
	}
	if len(accountChoices.Accounts) == 0 {
		return
	}
	if err := warp.SaveAccountModelChoices(ctx, s, accountChoices); err != nil {
		// Model refresh should still succeed when the advisory account/model
		// cache cannot be written.
		return
	}
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
	case "warp", "puter":
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

	defaultModelID := chooseRefreshedDefaultModel(channel, existingByID, candidates)
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
	}
	if shouldForceWarpDefault(channel, defaultModelID) {
		if existing := existingByID[defaultModelID]; existing != nil && !existing.IsDefault {
			updated := *existing
			updated.IsDefault = true
			if err := s.UpdateModel(ctx, &updated); err != nil {
				return nil, err
			}
			result.Updated++
		}
	}

	if shouldDeleteMissingModelsOnRefresh(channel, source) {
		for modelID, existing := range existingByID {
			if _, ok := fetchedSet[modelID]; ok {
				continue
			}
			if existing == nil || existing.ID == "" {
				continue
			}
			if err := s.DeleteModel(ctx, existing.ID); err != nil {
				return nil, err
			}
			result.Deleted++
			result.DeletedModelIDs = append(result.DeletedModelIDs, modelID)
		}
	}

	sort.Strings(result.AddedModelIDs)
	sort.Strings(result.DeletedModelIDs)
	sort.Strings(result.OfflineModelIDs)
	return result, nil
}

func shouldDeleteMissingModelsOnRefresh(channel, source string) bool {
	return strings.EqualFold(strings.TrimSpace(channel), "warp") &&
		strings.Contains(strings.TrimSpace(source), "agent_mode_llms")
}

func chooseRefreshedDefaultModel(channel string, existing map[string]*store.Model, ordered []discoveredModel) string {
	if strings.EqualFold(strings.TrimSpace(channel), "warp") && discoveredModelsContain(ordered, warpDefaultModelID()) {
		return warpDefaultModelID()
	}
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

func warpDefaultModelID() string {
	return "auto-open"
}

func shouldForceWarpDefault(channel, modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(channel), "warp") && modelID == warpDefaultModelID()
}

func discoveredModelsContain(models []discoveredModel, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
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
