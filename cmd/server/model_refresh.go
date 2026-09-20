package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/warp"
	"orchids-api/internal/workbuddy"
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
	Skipped         bool     `json:"skipped,omitempty"`
	DefaultModelID  string   `json:"default_model_id,omitempty"`
	AddedModelIDs   []string `json:"added_model_ids,omitempty"`
	DeletedModelIDs []string `json:"deleted_model_ids,omitempty"`
	OfflineModelIDs []string `json:"offline_model_ids,omitempty"`
}

type discoveredModel struct {
	ID        string
	Name      string
	SortOrder int
	// Verified marks a candidate this refresh actually observed as usable
	// upstream (a catalog read that succeeded, or a probe that was accepted).
	// It is set per candidate rather than inferred from the candidate count, so
	// "discovered" and "verified" stay distinguishable in the admin report.
	Verified bool
}

// noActiveAccountsError reports that a channel has no account eligible for an
// upstream catalog read. Model management publishes nothing in that state:
// every published row has to be an observation of an upstream catalog, never a
// locally compiled-in default.
type noActiveAccountsError struct {
	Channel string
}

func (e *noActiveAccountsError) Error() string {
	return fmt.Sprintf("%s has no active account; model refresh only publishes upstream catalogs", e.Channel)
}

func isNoActiveAccounts(err error) bool {
	var target *noActiveAccountsError
	return errors.As(err, &target)
}

// upstreamCatalogSources are the only refresh sources allowed to publish model
// rows. Each one names a catalog that was read from an upstream service for an
// account that is currently active. A cached list, an unverified public list or
// a compiled-in catalog is not an observation and must not reach the store.
//
// The match is exact rather than by prefix: "grok_build_models" is an
// observation while "grok_build_models_unavailable_cached" is not, and a prefix
// test would accept the latter.
var upstreamCatalogSources = map[string]struct{}{
	"grok_build_models":             {},
	"workbuddy_cli_models":          {},
	"qoder_upstream_models":         {},
	"puter_public_models_test_mode": {},
}

// warpGraphQLSourcePrefix is the stable prefix of the Warp catalog source, which
// names the GraphQL fields that answered and therefore carries a dynamic suffix.
const warpGraphQLSourcePrefix = "warp_graphql_"

func isUpstreamCatalogSource(source string) bool {
	source = strings.TrimSpace(source)
	if _, ok := upstreamCatalogSources[source]; ok {
		return true
	}
	return strings.HasPrefix(source, warpGraphQLSourcePrefix)
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
			// No active account is a legitimate state, not a failure: nothing was
			// fetched, so nothing is published. It is reported as a skipped
			// refresh so the admin page can distinguish it from an upstream
			// outage and from a successful empty refresh.
			if isNoActiveAccounts(err) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(&modelRefreshResult{
					Channel:     normalizeAdminModelChannel(channel),
					Source:      "no_active_account",
					Concurrency: concurrency,
					Skipped:     true,
				})
				return
			}
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
	case "workbuddy":
		return "WorkBuddy"
	case "qoder":
		return "Qoder"
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
	case "workbuddy":
		return discoverWorkBuddyModels(ctx, cfg, s)
	case "qoder":
		return discoverQoderModels(ctx, cfg, s)
	case "grok":
		return discoverGrokModelsConcurrent(ctx, cfg, s, concurrency)
	default:
		return nil, "", fmt.Errorf("unsupported channel: %s", channel)
	}
}

// discoverWorkBuddyModels reads the account-scoped WorkBuddy model catalog.
// GET /v3/config is an authenticated control-plane endpoint, so a successful
// read is itself proof that the credential works; no completion probe is sent
// (the upstream bills per token, unlike Puter's free test_mode).
func discoverWorkBuddyModels(ctx context.Context, cfg *config.Config, s *store.Store) ([]discoveredModel, string, error) {
	source := "workbuddy_cli_models"
	accounts, err := enabledAccountsByType(ctx, s, "workbuddy")
	if err != nil {
		return nil, "", fmt.Errorf("workbuddy model discovery failed: %w", err)
	}
	if len(accounts) == 0 {
		return nil, "", &noActiveAccountsError{Channel: "WorkBuddy"}
	}

	var lastErr error
	for _, acc := range accounts {
		client := workbuddy.NewFromAccount(acc, refreshModelRequestConfig(cfg, "workbuddy"))
		models, fetchErr := client.FetchModels(ctx)
		client.Close()
		if fetchErr != nil {
			lastErr = fetchErr
			continue
		}
		candidates := workBuddyCatalogToDiscovered(models)
		if len(candidates) == 0 {
			lastErr = fmt.Errorf("workbuddy account #%d returned an empty cli catalog", acc.ID)
			continue
		}
		persistWorkBuddyCatalogSnapshot(ctx, s, acc, models)
		return candidates, source, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("workbuddy model discovery failed")
	}
	return nil, "", fmt.Errorf("workbuddy model discovery failed: %w", lastErr)
}

// discoverQoderModels publishes the Qoder channel catalog read from the signed
// upstream control plane.
//
// There is no local fallback. This channel previously published a compiled-in
// catalog because the model-list read was rejected; a built-in list is not an
// observation of what the account may run, so the refresh now reports the read
// failure instead of restating a compiled-in default as discovered state.
func discoverQoderModels(ctx context.Context, cfg *config.Config, s *store.Store) ([]discoveredModel, string, error) {
	source := "qoder_upstream_models"
	accounts, err := enabledAccountsByType(ctx, s, "qoder")
	if err != nil {
		return nil, "", fmt.Errorf("qoder model discovery failed: %w", err)
	}
	if len(accounts) == 0 {
		return nil, "", &noActiveAccountsError{Channel: "Qoder"}
	}

	var lastErr error
	for _, acc := range accounts {
		// The catalog read is the only thing that may publish Qoder rows, and it
		// never sends a chat request.
		client := qoder.NewFromAccount(acc, refreshModelRequestConfig(cfg, "qoder"))
		catalog, fetchErr := client.FetchUpstreamModels(ctx)
		client.Close()
		if fetchErr != nil {
			lastErr = fetchErr
			continue
		}
		candidates := qoderCatalogToDiscovered(catalog)
		if len(candidates) == 0 {
			lastErr = fmt.Errorf("qoder account #%d returned an empty upstream catalog", acc.ID)
			continue
		}
		persistQoderCatalogSnapshot(ctx, s, acc, catalog)
		return candidates, source, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("qoder model discovery failed")
	}
	return nil, "", fmt.Errorf("qoder model discovery failed: %w", lastErr)
}

// qoderCatalogToDiscovered maps the account catalog onto the channel's public
// model records.
//
// The public identifier is the display name, not the internal gateway key: the
// channel resolves either form, and a client that saw "Qwen3.7-Max" in
// /v1/models must be able to ask for it by that name. The internal key stays in
// the account snapshot, which is what the client resolves against.
func qoderCatalogToDiscovered(catalog *qoder.Catalog) []discoveredModel {
	entries := catalog.Entries()
	out := make([]discoveredModel, 0, len(entries))
	for i, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		id := strings.TrimSpace(entry.Name)
		if id == "" {
			id = key
		}
		// Model lookup is lowercased before it reaches the store index, so the
		// public identifier is stored in lowercase. The upstream key and the
		// display name are both still accepted at request time, because the
		// catalog resolves case-insensitively.
		id = strings.ToLower(id)
		out = append(out, discoveredModel{ID: id, Name: id, SortOrder: i, Verified: true})
	}
	return out
}

// persistQoderCatalogSnapshot records the account-scoped upstream catalog so
// model selection resolves against the same list the channel publishes.
func persistQoderCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, catalog *qoder.Catalog) {
	if acc == nil || acc.ID == 0 {
		return
	}
	ids := qoder.CatalogSnapshot(catalog)
	if len(ids) == 0 {
		return
	}
	acc.QoderModelIDs = ids
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("failed to persist qoder model snapshot", "account_id", acc.ID, "error", err)
	}
}

func workBuddyCatalogToDiscovered(models []workbuddy.WorkBuddyModel) []discoveredModel {
	out := make([]discoveredModel, 0, len(models))
	for i, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: i, Verified: true})
	}
	return out
}

// persistWorkBuddyCatalogSnapshot records the account-scoped whitelist so model
// selection can be checked against what this account may actually run. Each row
// keeps the window the catalog declared, because the public model list has to
// report a real number for a client that budgets its context.
func persistWorkBuddyCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, models []workbuddy.WorkBuddyModel) {
	if acc == nil || acc.ID == 0 {
		return
	}
	ids := workbuddy.CatalogSnapshot(models)
	if len(ids) == 0 {
		return
	}
	acc.WorkBuddyModelIDs = ids
	acc.WorkBuddyModelsSyncedAt = time.Now()
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("failed to persist workbuddy model snapshot", "account_id", acc.ID, "error", err)
	}
}

func discoverPuterModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	// Puter's public catalog is readable without a credential, but a published
	// model is only trustworthy when an active account accepted it. Without an
	// active account the refresh therefore observes nothing and publishes
	// nothing.
	accounts, accErr := enabledAccountsByType(ctx, s, "puter")
	if accErr != nil {
		return nil, "", fmt.Errorf("puter model discovery failed: %w", accErr)
	}
	if len(accounts) == 0 {
		return nil, "", &noActiveAccountsError{Channel: "Puter"}
	}

	proxyFunc := http.ProxyFromEnvironment
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
	}
	items, err := fetchPuterPublicModelChoices(ctx, proxyFunc)
	if err != nil {
		return nil, "", fmt.Errorf("puter public model discovery failed: %w", err)
	}
	if len(items) == 0 {
		return nil, "", fmt.Errorf("puter public model discovery returned no choices")
	}

	candidates := puterChoicesToDiscovered(items)
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("puter has no discoverable models")
	}

	summary := verifyPuterDiscoveredModelsConcurrent(ctx, cfg, accounts, candidates, concurrency)
	verified := summary.Verified
	if len(verified) == 0 && summary.SawInsufficientFunds {
		return nil, "", fmt.Errorf("puter accounts reported insufficient funds; no model could be observed as available")
	}
	if len(verified) == 0 {
		return nil, "", fmt.Errorf("no puter models verified by test_mode")
	}
	return verified, "puter_public_models_test_mode", nil
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
		candidate.Verified = true
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
			candidate.Verified = true
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
// A failed control-plane read publishes nothing. The historical catalog is not
// a fallback: the rows already in the store are last known state, not a new
// observation, and re-publishing them would report a stale catalog as freshly
// discovered.
func discoverGrokModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	accounts, err := grokBuildModelDiscoveryAccounts(ctx, s)
	if err != nil {
		return nil, "", err
	}
	if len(accounts) == 0 {
		return nil, "", &noActiveAccountsError{Channel: "Grok"}
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
	var lastErr error
	for _, result := range ordered {
		if result.account == nil || result.err != nil || len(result.models) == 0 {
			if result.err != nil {
				lastErr = result.err
			}
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
			merged = append(merged, discoveredModel{ID: spec.ID, Name: util.FirstNonEmpty(spec.Name, spec.ID), SortOrder: len(merged), Verified: true})
		}
	}
	if len(merged) > 0 {
		return merged, "grok_build_models", nil
	}
	if successes > 0 {
		return nil, "", fmt.Errorf("official Grok Build catalog contains no locally routable models")
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("official Grok Build model discovery failed for all enabled OAuth accounts: %w", lastErr)
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

func discoverWarpModelsConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) ([]discoveredModel, string, error) {
	if s == nil {
		return nil, "", fmt.Errorf("store not configured")
	}

	// Model configuration is a control-plane read, but only an active account
	// may supply it: a disabled account is not part of the pool this gateway
	// serves from, and publishing its catalog would advertise models no request
	// can actually be routed to.
	accounts, err := warpModelDiscoveryAccounts(ctx, s)
	if err != nil {
		return nil, "", err
	}
	if len(accounts) == 0 {
		return nil, "", &noActiveAccountsError{Channel: "Warp"}
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
			Verified:  true,
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
	// Warp can temporarily hide every agent-mode choice while a workspace is
	// still provisioning. The last verified global catalog stays in the store as
	// last known state, but this refresh observed nothing, so it publishes
	// nothing and reports the failure instead of restating the old rows as a new
	// discovery.
	return nil, "", fmt.Errorf("warp model discovery returned no account choices")
}

// warpModelDiscoveryAccounts returns the Warp accounts a catalog read may use.
// Only enabled, credential-bearing accounts qualify: a disabled account is not
// in the serving pool, so its catalog is not this deployment's catalog.
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
		if !acc.Enabled {
			continue
		}
		if strings.TrimSpace(warp.RefreshToken(acc)) == "" {
			continue
		}
		eligible = append(eligible, acc)
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	return eligible, nil
}

func saveWarpAccountModelChoices(ctx context.Context, s *store.Store, discoveries []warpAccountDiscovery) {
	if s == nil {
		return
	}
	// Start from what is already stored rather than from an empty struct: this
	// function writes the whole setting, so rebuilding it from scratch would drop
	// the window table every account that failed discovery in this pass.
	accountChoices, err := warp.LoadAccountModelChoices(ctx, s)
	if err != nil {
		slog.Warn("warp account model choices could not be read; rebuilding", "error", err)
		accountChoices = nil
	}
	if accountChoices == nil {
		accountChoices = &warp.AccountModelChoices{}
	}
	if accountChoices.Accounts == nil {
		accountChoices.Accounts = make(map[string][]string)
	}
	if accountChoices.Sources == nil {
		accountChoices.Sources = make(map[string]string)
	}
	if accountChoices.FeatureConfigs == nil {
		accountChoices.FeatureConfigs = make(map[string]warp.AccountFeatureConfig)
	}
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
		// The input window belongs to the model, not to the account, so every
		// account's discovery feeds one shared table. Dropping it here is what
		// left the request builder with no window to state upstream.
		accountChoices.ContextWindows = warp.MergeContextWindows(accountChoices.ContextWindows, warp.ContextWindowsFromChoices(result.choices))
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
	for _, part := range []string{"feature_model_choice_all", "feature_model_choice_agent_mode"} {
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
	case "warp", "puter", "workbuddy", "qoder":
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
	// The single gate that keeps locally compiled-in or cached catalogs out of
	// model management. Discovery is expected to fail instead of returning a
	// non-upstream source; this refuses the write if it ever does.
	if !isUpstreamCatalogSource(source) {
		return nil, fmt.Errorf("%s refresh refused: %q is not an upstream catalog source", channel, source)
	}

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
		if model.Verified {
			result.Verified++
		}
	}

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
		if existing != nil {
			// A refresh that observed this model is what marks it verified.
			// Creation alone was not enough: a row that predates the observation
			// kept Verified=false forever, and an unverified Grok row is not
			// visible, so the channel's own default model disappeared from
			// /v1/models. Only the flag is touched — name, status, ordering and
			// default are operator-owned and stay as they are.
			if model.Verified && !existing.Verified {
				updated := *existing
				updated.Verified = true
				if err := s.UpdateModel(ctx, &updated); err != nil {
					return nil, err
				}
				result.Updated++
			}
			continue
		}
		record := &store.Model{
			Channel:   channel,
			ModelID:   model.ID,
			Name:      util.FirstNonEmpty(model.Name, model.ID),
			Status:    store.ModelStatusAvailable,
			Verified:  model.Verified,
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

// shouldDeleteMissingModelsOnRefresh reports whether a refresh may prune rows
// that the upstream catalog no longer advertises.
//
// Pruning is only allowed on a source that is authoritative for the whole
// channel. It is refused for two different reasons:
//
//   - A non-upstream source observed nothing, so a missing row is not evidence.
//   - The Grok Build OAuth read is a *text-model* catalog. It deliberately omits
//     the Composer capability and every media, voice and STT route the channel
//     implements, so a row missing from it may still be perfectly routable.
//     Pruning on it deleted 16 working models in production, which is why the
//     Build source is excluded here.
func shouldDeleteMissingModelsOnRefresh(channel, source string) bool {
	source = strings.TrimSpace(source)
	if source == "grok_build_models" {
		return false
	}
	return isUpstreamCatalogSource(source)
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
