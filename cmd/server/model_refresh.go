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

// fetchGrokBuildModelsForRefresh reads the official Build CLI catalog.  It is
// deliberately kept as an injectable control-plane operation: model refresh
// must never send a completion simply to discover an account's capabilities.
var fetchGrokBuildModelsForRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) ([]string, error) {
	client := grok.NewCLIClient(cfg)
	client.SetAccountStore(s)
	return client.FetchModels(ctx, acc)
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
	index         int
	id            int64
	choices       []warp.ModelChoice
	source        string
	featureConfig warp.AccountFeatureConfig
	ok            bool
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

func discoverModelsForChannelConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) ([]discoveredModel, string, error) {
	switch strings.ToLower(channel) {
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
		if err != nil {
			return nil, "", fmt.Errorf("puter public model discovery failed: %w", err)
		}
		return nil, "", fmt.Errorf("puter public model discovery returned no choices")
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

	summary := verifyPuterDiscoveredModelsConcurrent(ctx, cfg, accounts, candidates, concurrency)
	verified := summary.Verified
	if len(verified) == 0 && summary.SawInsufficientFunds {
		return candidates, source + "_quota_limited", nil
	}
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

type puterModelVerificationSummary struct {
	Verified             []discoveredModel
	SawInsufficientFunds bool
}

func verifyPuterDiscoveredModelsConcurrent(ctx context.Context, cfg *config.Config, accounts []*store.Account, candidates []discoveredModel, concurrency int) puterModelVerificationSummary {
	if len(accounts) == 0 || len(candidates) == 0 {
		return puterModelVerificationSummary{}
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
					if isPuterInsufficientFundsError(err) {
						results[idx] = results[idx].withInsufficientFunds()
					}
					if !isPuterModelDefinitiveReject(err) {
						allDefinitiveRejects = false
					}
				}
				if results[idx] != puterModelProbeAccepted && results[idx] != puterModelProbeQuotaLimited && allDefinitiveRejects {
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
	sawInsufficientFunds := false
	for idx, candidate := range candidates {
		if results[idx] == puterModelProbeQuotaLimited {
			sawInsufficientFunds = true
		}
		if results[idx] != puterModelProbeAccepted {
			continue
		}
		candidate.SortOrder = len(verified)
		verified = append(verified, candidate)
	}
	return puterModelVerificationSummary{Verified: verified, SawInsufficientFunds: sawInsufficientFunds}
}

func verifyPuterDiscoveredModelsSerial(ctx context.Context, cfg *config.Config, accounts []*store.Account, candidates []discoveredModel) puterModelVerificationSummary {
	verified := make([]discoveredModel, 0, len(candidates))
	sawInsufficientFunds := false
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
			if isPuterInsufficientFundsError(err) {
				sawInsufficientFunds = true
			}
		}
		if ok {
			candidate.SortOrder = len(verified)
			verified = append(verified, candidate)
		}
	}
	return puterModelVerificationSummary{Verified: verified, SawInsufficientFunds: sawInsufficientFunds}
}

type puterModelProbeResult uint8

const (
	_ puterModelProbeResult = iota
	puterModelProbeAccepted
	puterModelProbeRejected
	puterModelProbeQuotaLimited
)

func (r puterModelProbeResult) withInsufficientFunds() puterModelProbeResult {
	if r == puterModelProbeAccepted {
		return r
	}
	return puterModelProbeQuotaLimited
}

func isPuterInsufficientFundsError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(text, "insufficient_funds") ||
		strings.Contains(text, "available funding is insufficient") ||
		strings.Contains(text, "insufficient funding") ||
		strings.Contains(text, "status=402") ||
		strings.Contains(text, "status 402")
}

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

type grokBuildModelDiscovery struct {
	index   int
	account *store.Account
	models  []string
	err     error
}

// discoverGrokModelsConcurrent makes the Build OAuth /v1/models catalog the
// primary source of Grok text-model discovery.  The upstream response is
// account scoped, so every successful response is persisted on that account;
// only models with a locally implemented Build route are published globally.
//
// A failed control-plane read must never erase the last known global catalog.
// The historical catalog is consequently used only as an outage/no-account
// fallback, never merged into a successful Build discovery.
func discoverGrokModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	accounts, err := grokBuildModelDiscoveryAccounts(ctx, s)
	if err != nil {
		return nil, "", err
	}
	if len(accounts) == 0 {
		if cached := cachedGrokModels(ctx, s); len(cached) > 0 {
			return cached, "grok_cached_models", nil
		}
		return nil, "", fmt.Errorf("no enabled Grok Build OAuth accounts or cached models")
	}

	workerCount := boundedModelRefreshWorkers(len(accounts), concurrency)
	jobs := make(chan int, len(accounts))
	results := make(chan grokBuildModelDiscovery, len(accounts))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				acc := accounts[index]
				models, fetchErr := fetchGrokBuildModelsForRefresh(ctx, cfg, s, acc)
				results <- grokBuildModelDiscovery{index: index, account: acc, models: models, err: fetchErr}
			}
		}()
	}
	for index := range accounts {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	close(results)

	ordered := make([]grokBuildModelDiscovery, len(accounts))
	for result := range results {
		if result.index >= 0 && result.index < len(ordered) {
			ordered[result.index] = result
		}
	}

	now := time.Now().UTC()
	merged := make([]discoveredModel, 0, len(accounts)*2)
	seen := make(map[string]struct{})
	successes := 0
	for _, result := range ordered {
		if result.account == nil || result.err != nil || len(result.models) == 0 {
			continue
		}
		successes++
		// Persist the full official account capability snapshot, including an
		// ID that this gateway intentionally does not route yet.  That keeps
		// capability truth separate from the public compatibility surface.
		grok.NormalizeProvider(result.account)
		grok.ApplyCLIModels(result.account, result.models, now)
		if updateErr := s.UpdateAccount(ctx, result.account); updateErr != nil {
			return nil, "", fmt.Errorf("persist grok build model catalog: %w", updateErr)
		}
		// Publish the normalized capability view as well: Build intentionally
		// omits stable Composer and compatibility aliases from sparse /models
		// responses even though the account can route them.
		for _, rawID := range result.account.GrokModels {
			id := canonicalGrokRefreshModelID(rawID)
			spec, ok := grok.ResolveModel(id)
			if !ok {
				// Build's account catalog is the source of truth. Unknown but
				// advertised IDs are published as dynamic Build routes.
				spec = grok.ModelSpec{ID: id, Name: id, UpstreamModel: id, Upstream: grok.UpstreamCLI}
			} else if spec.Upstream != grok.UpstreamCLI {
				continue
			}
			key := strings.ToLower(id)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, discoveredModel{ID: spec.ID, Name: util.FirstNonEmpty(spec.Name, spec.ID), SortOrder: len(merged)})
		}
	}
	if len(merged) > 0 {
		return merged, "grok_build_models", nil
	}
	if cached := cachedGrokModels(ctx, s); len(cached) > 0 {
		if successes > 0 {
			return cached, "grok_build_models_no_routable_models_cached", nil
		}
		return cached, "grok_build_models_unavailable_cached", nil
	}
	if successes > 0 {
		return nil, "", fmt.Errorf("official Grok Build catalog contains no locally routable models")
	}
	return nil, "", fmt.Errorf("official Grok Build model discovery failed for all enabled OAuth accounts")
}

func canonicalGrokRefreshModelID(modelID string) string {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return ""
	}
	// Video 1.5 exists on both Console and Build. A capability discovered from
	// the Build control plane must retain its provider-qualified route instead
	// of resolving to the unprefixed Console compatibility model.
	if strings.EqualFold(id, "grok-imagine-video-1.5") {
		return "build/grok-imagine-video-1.5"
	}
	if spec, ok := grok.ResolveModel(id); ok {
		return spec.ID
	}
	return id
}

func grokBuildModelDiscoveryAccounts(ctx context.Context, s *store.Store) ([]*store.Account, error) {
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || grok.ProviderForAccount(acc) != grok.ProviderBuild || !strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
			continue
		}
		if strings.TrimSpace(acc.OAuthAccessToken) == "" && strings.TrimSpace(acc.OAuthRefreshToken) == "" {
			continue
		}
		out = append(out, acc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func cachedGrokModels(ctx context.Context, s *store.Store) []discoveredModel {
	if s == nil {
		return nil
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]discoveredModel, 0, len(models))
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "grok") {
			continue
		}
		id := canonicalGrokRefreshModelID(model.ModelID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		name := util.FirstNonEmpty(model.Name, id)
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: len(out)})
	}
	return out
}

func discoverWarpModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	if s == nil {
		return nil, "", fmt.Errorf("store not configured")
	}

	// Model configuration is read-only metadata, not an inference request.
	// A quota-exhausted account is usually disabled for routing but can still
	// expose its official model choices. Prefer enabled accounts, then use a
	// credential-bearing disabled account as a catalog fallback so the global
	// model-management page does not become unusable when every paid account is
	// cooling down or exhausted.
	accounts, err := warpModelDiscoveryAccounts(ctx, s)
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
		name := util.FirstNonEmpty(choice.Name, id)
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
					features, source, discoverErr := client.FetchDiscoveredFeatureModelChoices(ctx)
					client.Close()
					if discoverErr != nil {
						continue
					}
					choices := warp.AgentModeModelChoices(features)
					featureConfig := warp.AccountFeatureConfigFromChoices(features)
					if warp.AccountFreeOnly(acc) {
						choices = probeWarpFreeOnlyModelChoices(ctx, cfg, acc, choices)
						source = appendWarpDiscoverySource(source, "free_probe")
					}
					if len(choices) == 0 {
						continue
					}
					results <- warpAccountDiscovery{
						index:         idx,
						id:            acc.ID,
						choices:       choices,
						source:        source,
						featureConfig: featureConfig,
						ok:            true,
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
	// Warp can temporarily hide every agent-mode choice when all accounts are
	// exhausted or a workspace is still provisioning. Do not turn that missing
	// catalog into a destructive refresh. Keep the last verified global catalog
	// visible and report its source explicitly; a later refresh will replace it
	// as soon as GraphQL returns choices again.
	if cached := cachedWarpModels(ctx, s); len(cached) > 0 {
		return cached, "warp_cached_models", nil
	}
	return nil, "", fmt.Errorf("warp model discovery returned no account choices")
}

func cachedWarpModels(ctx context.Context, s *store.Store) []discoveredModel {
	if s == nil {
		return nil
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil
	}
	out := make([]discoveredModel, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "warp") {
			continue
		}
		id := warp.NormalizeModelID(model.ModelID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: len(out)})
	}
	return out
}

func warpModelDiscoveryAccounts(ctx context.Context, s *store.Store) ([]*store.Account, error) {
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	eligible := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
			continue
		}
		if strings.TrimSpace(warp.RefreshToken(acc)) == "" {
			continue
		}
		eligible = append(eligible, acc)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Enabled != eligible[j].Enabled {
			return eligible[i].Enabled
		}
		return eligible[i].ID < eligible[j].ID
	})
	return eligible, nil
}

func probeWarpFreeOnlyModelChoices(ctx context.Context, cfg *config.Config, acc *store.Account, discovered []warp.ModelChoice) []warp.ModelChoice {
	candidates := warpFreeOnlyProbeCandidates(discovered)
	out := make([]warp.ModelChoice, 0, len(candidates))
	for _, choice := range candidates {
		if err := probeWarpModelForRefresh(ctx, cfg, acc, choice.ID); err != nil {
			continue
		}
		out = append(out, choice)
	}
	return out
}

func appendWarpDiscoverySource(parts ...string) string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, sub := range strings.Split(part, "+") {
			sub = strings.TrimSpace(sub)
			if sub == "" {
				continue
			}
			if _, exists := seen[sub]; exists {
				continue
			}
			seen[sub] = struct{}{}
			out = append(out, sub)
		}
	}
	return strings.Join(out, "+")
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
		if !ok {
			choice = warp.ModelChoice{ID: id, Name: warpProbeModelName(id)}
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

func warpProbeModelName(id string) string {
	switch id {
	case warp.DefaultModel():
		return "Warp Auto Open"
	case "claude-4-5-haiku":
		return "Claude 4.5 Haiku"
	case "claude-4-5-sonnet":
		return "Claude 4.5 Sonnet"
	case "claude-4-5-opus":
		return "Claude 4.5 Opus"
	case "gpt-5-2-low":
		return "GPT-5.2 Low"
	case "gpt-5-1-low":
		return "GPT-5.1 Low"
	case "gemini-3-5-flash":
		return "Gemini 3.5 Flash"
	default:
		return id
	}
}

func saveWarpAccountModelChoices(ctx context.Context, s *store.Store, discoveries []warpAccountDiscovery) {
	if s == nil {
		return
	}
	accountChoices := &warp.AccountModelChoices{Accounts: make(map[string][]string)}
	accountChoices.Sources = make(map[string]string)
	accountChoices.FeatureConfigs = make(map[string]warp.AccountFeatureConfig)
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
		if !result.featureConfig.IsEmpty() {
			accountChoices.FeatureConfigs[key] = result.featureConfig
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

func joinWarpDiscoverySources(sourceSet map[string]struct{}) string {
	if len(sourceSet) == 0 {
		return "warp_graphql"
	}
	ordered := make([]string, 0, 1)
	for _, part := range []string{"feature_model_choice_all", "feature_model_choice_agent_mode", "free_probe"} {
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
				Name:      util.FirstNonEmpty(model.Name, model.ID),
				Status:    store.ModelStatusAvailable,
				Verified:  true,
				IsDefault: model.ID == defaultModelID,
				SortOrder: model.SortOrder,
			}
			if strings.EqualFold(strings.TrimSpace(channel), "grok") {
				store.ApplyGrokRouteDefaults(record)
				record.Origin = "discovery"
				record.Provider = grok.ProviderBuild
				record.UpstreamModel = model.ID
				if strings.Contains(strings.ToLower(model.ID), "video") {
					record.Capabilities = []string{store.CapabilityVideo}
				} else {
					record.Capabilities = []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses}
				}
				record.NormalizeRoute()
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
	if strings.EqualFold(strings.TrimSpace(channel), "puter") {
		return strings.HasPrefix(strings.TrimSpace(source), "puter_public_models")
	}
	if !strings.EqualFold(strings.TrimSpace(channel), "warp") {
		return false
	}
	source = strings.TrimSpace(source)
	return strings.Contains(source, "feature_model_choice_agent_mode") || strings.Contains(source, "feature_model_choice_all")
}

func chooseRefreshedDefaultModel(channel string, existing map[string]*store.Model, ordered []discoveredModel) string {
	if strings.EqualFold(strings.TrimSpace(channel), "warp") && discoveredModelsContain(ordered, warpDefaultModelID) {
		return warpDefaultModelID
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

const warpDefaultModelID = "auto-open"

func shouldForceWarpDefault(channel, modelID string) bool {
	return strings.EqualFold(strings.TrimSpace(channel), "warp") && modelID == warpDefaultModelID
}

func discoveredModelsContain(models []discoveredModel, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}
