package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestMakeModelRefreshHandler_UsesBodyChannel(t *testing.T) {
	prev := runModelRefresh
	defer func() { runModelRefresh = prev }()

	runModelRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error) {
		return &modelRefreshResult{Channel: channel, Source: "stub", Concurrency: concurrency, Discovered: 3, Verified: 2}, nil
	}

	handler := makeModelRefreshHandler(&config.Config{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=warp&concurrency=99", strings.NewReader(`{"channel":"puter","concurrency":8}`))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	var resp modelRefreshResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Channel != "puter" {
		t.Fatalf("channel=%q want %q", resp.Channel, "puter")
	}
	if resp.Verified != 2 {
		t.Fatalf("verified=%d want 2", resp.Verified)
	}
	if resp.Concurrency != 8 {
		t.Fatalf("concurrency=%d want 8", resp.Concurrency)
	}
}

func TestNormalizeModelRefreshConcurrency(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "default on zero", in: 0, want: defaultModelRefreshConcurrency},
		{name: "default on negative", in: -2, want: defaultModelRefreshConcurrency},
		{name: "keeps valid", in: 8, want: 8},
		{name: "clamps max", in: 99, want: maxModelRefreshConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeModelRefreshConcurrency(tt.in); got != tt.want {
				t.Fatalf("normalizeModelRefreshConcurrency(%d)=%d want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseModelRefreshConcurrency(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "", want: 0, ok: false},
		{raw: "2", want: 2, ok: true},
		{raw: "99", want: maxModelRefreshConcurrency, ok: true},
		{raw: "bad", want: defaultModelRefreshConcurrency, ok: true},
	}
	for _, tt := range tests {
		got, ok := parseModelRefreshConcurrency(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("parseModelRefreshConcurrency(%q)=(%d,%v) want (%d,%v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSyncModelsForChannel_SkipsVerificationAndUsesDiscoveryList(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Warp")

	result, err := syncModelsForChannel(ctx, &config.Config{}, s, "Warp")
	if err != nil {
		t.Fatalf("syncModelsForChannel() error = %v", err)
	}
	if result.Discovered == 0 {
		t.Fatal("expected discovered warp models to be non-empty")
	}
	if result.Verified != result.Discovered {
		t.Fatalf("verified=%d want discovered=%d", result.Verified, result.Discovered)
	}
	if result.Concurrency != defaultModelRefreshConcurrency {
		t.Fatalf("concurrency=%d want %d", result.Concurrency, defaultModelRefreshConcurrency)
	}
}

func TestSyncModelsForChannelConcurrent_RecordsConcurrency(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Warp")

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Warp", 8)
	if err != nil {
		t.Fatalf("syncModelsForChannelConcurrent() error = %v", err)
	}
	if result.Concurrency != 8 {
		t.Fatalf("concurrency=%d want 8", result.Concurrency)
	}
}

func TestChooseRefreshedDefaultModel_PrefersExistingDefault(t *testing.T) {
	existing := map[string]*store.Model{
		"a": {ModelID: "a", IsDefault: true},
		"b": {ModelID: "b", IsDefault: false},
	}
	ordered := []discoveredModel{{ID: "b"}, {ID: "a"}}

	got := chooseRefreshedDefaultModel("Puter", existing, ordered)
	if got != "a" {
		t.Fatalf("default=%q want %q", got, "a")
	}
}

func TestChooseRefreshedDefaultModel_WarpPrefersAutoOpen(t *testing.T) {
	existing := map[string]*store.Model{
		"claude-4-5-opus": {ModelID: "claude-4-5-opus", IsDefault: true},
		"auto-open":       {ModelID: "auto-open", IsDefault: false},
	}
	ordered := []discoveredModel{{ID: "claude-4-5-opus"}, {ID: "auto-open"}}

	got := chooseRefreshedDefaultModel("Warp", existing, ordered)
	if got != "auto-open" {
		t.Fatalf("default=%q want auto-open", got)
	}
}

func TestDiscoverModelsForChannel_OrchidsUsesUpstreamAPI(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization=%q want Bearer test-token", got)
		}
		if got := r.URL.Path; got != "/v1/models" {
			t.Fatalf("path=%q want /v1/models", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-sonnet-4-6"},{"id":"claude-opus-4.6"},{"id":"claude-opus-4-6"},{"id":"gpt-5.4"}]}`))
	}))
	defer upstream.Close()

	if err := s.CreateAccount(ctx, &store.Account{
		AccountType: "orchids",
		Enabled:     true,
		Token:       "test-token",
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	items, source, err := discoverModelsForChannel(ctx, &config.Config{
		OrchidsAPIBaseURL: upstream.URL,
		RequestTimeout:    5,
	}, s, "Orchids")
	if err != nil {
		t.Fatalf("discoverModelsForChannel() error = %v", err)
	}
	if source != "upstream_api" {
		t.Fatalf("source=%q want %q", source, "upstream_api")
	}
	if len(items) != 3 {
		t.Fatalf("len(items)=%d want 3", len(items))
	}
	if items[0].ID != "claude-sonnet-4-6" || items[1].ID != "claude-opus-4-6" || items[2].ID != "gpt-5.4" {
		t.Fatalf("items=%+v want claude-sonnet-4-6,claude-opus-4-6,gpt-5.4", items)
	}
}

func TestVerifyPuterDiscoveredModelsConcurrent_RequiresAcceptedProbe(t *testing.T) {
	prevVerify := verifyPuterModelForRefresh
	t.Cleanup(func() { verifyPuterModelForRefresh = prevVerify })

	var mu sync.Mutex
	seen := map[string]int{}
	verifyPuterModelForRefresh = func(ctx context.Context, cfg *config.Config, acc *store.Account, modelID string) error {
		mu.Lock()
		seen[modelID]++
		mu.Unlock()
		switch modelID {
		case "stable":
			return nil
		case "flaky":
			return errors.New("puter API error: status=429, body=too many requests")
		case "missing":
			return errors.New("puter API error: message=Model not found, please try one of the following models listed here")
		default:
			return errors.New("failed to send puter verify request: timeout")
		}
	}

	got := verifyPuterDiscoveredModelsConcurrent(
		context.Background(),
		&config.Config{},
		[]*store.Account{{ID: 1, AccountType: "puter"}, {ID: 2, AccountType: "puter"}},
		[]discoveredModel{{ID: "stable"}, {ID: "flaky"}, {ID: "missing"}},
		8,
	)

	gotIDs := make([]string, 0, len(got))
	for _, item := range got {
		gotIDs = append(gotIDs, item.ID)
	}
	if strings.Join(gotIDs, ",") != "stable" {
		t.Fatalf("verified IDs=%v want [stable]", gotIDs)
	}
	if seen["missing"] != 2 {
		t.Fatalf("missing probes=%d want 2", seen["missing"])
	}
}

func TestVerifyPuterDiscoveredModelsSerial_RequiresAcceptedProbe(t *testing.T) {
	prevVerify := verifyPuterModelForRefresh
	t.Cleanup(func() { verifyPuterModelForRefresh = prevVerify })

	verifyPuterModelForRefresh = func(ctx context.Context, cfg *config.Config, acc *store.Account, modelID string) error {
		if modelID == "missing" {
			return errors.New("puter API error: message=Model not found, please try one of the following models listed here")
		}
		return errors.New("failed to send puter verify request: EOF")
	}

	got := verifyPuterDiscoveredModelsConcurrent(
		context.Background(),
		&config.Config{},
		[]*store.Account{{ID: 1, AccountType: "puter"}},
		[]discoveredModel{{ID: "flaky"}, {ID: "missing"}},
		1,
	)

	gotIDs := make([]string, 0, len(got))
	for _, item := range got {
		gotIDs = append(gotIDs, item.ID)
	}
	if strings.Join(gotIDs, ",") != "" {
		t.Fatalf("verified IDs=%v want []", gotIDs)
	}
}

func TestGrokCanonicalAcceptanceRejectsFallbackModels(t *testing.T) {
	tests := []struct {
		requested string
		canonical string
		want      bool
	}{
		{requested: "grok-4.20-0309", canonical: "grok-4.20-0309", want: true},
		{requested: "grok-4.3", canonical: "grok-4.3", want: true},
		{requested: "grok-4.3-latest", canonical: "grok-4.3", want: false},
		{requested: "grok-latest", canonical: "grok-4.3", want: false},
		{requested: "grok-3-mini", canonical: "grok-4.20-0309", want: false},
		{requested: "grok-420", canonical: "grok-4.20-0309", want: false},
	}
	for _, tt := range tests {
		if got := isAcceptedGrokCanonical(tt.requested, tt.canonical); got != tt.want {
			t.Fatalf("isAcceptedGrokCanonical(%q,%q)=%v want %v", tt.requested, tt.canonical, got, tt.want)
		}
	}
}

func TestGrokProbeCandidatesIncludesPolicyAndExistingModels(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel:   "Grok",
		ModelID:   "grok-5",
		Name:      "grok-5",
		Status:    store.ModelStatusAvailable,
		Verified:  true,
		IsDefault: false,
		SortOrder: 99,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	items := grokProbeCandidateModels(context.Background(), s)
	gotSet := make(map[string]struct{}, len(items))
	for _, item := range items {
		gotSet[item.ID] = struct{}{}
	}
	for _, publicID := range modelpolicy.PublicGrokModelIDs() {
		if _, ok := gotSet[publicID]; !ok {
			t.Fatalf("expected candidates to include public model %q, got %+v", publicID, items)
		}
	}
	if _, ok := gotSet["grok-5"]; !ok {
		t.Fatalf("expected candidates to include existing model grok-5, got %+v", items)
	}
}

func TestCanonicalizeDiscoveredModels_NormalizesLegacyGrok43(t *testing.T) {
	got := canonicalizeDiscoveredModels([]discoveredModel{
		{ID: "grok-4.3", Name: "Grok 4.3"},
		{ID: "grok-4.3", Name: "Grok 4.3"},
	}, canonicalGrokRefreshModelID)
	if len(got) != 1 {
		t.Fatalf("len(got)=%d want 1: %+v", len(got), got)
	}
	if got[0].ID != "grok-4.3" {
		t.Fatalf("ID=%q want grok-4.3", got[0].ID)
	}
	if got[0].Name != "Grok 4.3" {
		t.Fatalf("Name=%q want Grok 4.3", got[0].Name)
	}
}

func TestGrokConsoleModelsRemainAcceptedAfterProbeFallback(t *testing.T) {
	candidates := []discoveredModel{{ID: "grok-4.3", Name: "Grok 4.3"}}
	accepted := make([]bool, len(candidates))
	for idx, candidate := range candidates {
		if spec, ok := grok.ResolveModel(candidate.ID); ok && strings.TrimSpace(spec.ConsoleModel) != "" {
			accepted[idx] = true
		}
	}
	if !accepted[0] {
		t.Fatal("expected grok-4.3 to remain accepted as a console model")
	}
}

func TestApplyModelRefresh_PreservesModelsMissingFromUnreliableDiscoveredList(t *testing.T) {
	testCases := []struct {
		channel string
		modelID string
	}{
		{channel: "Puter", modelID: "puter-unavailable-model"},
		{channel: "Orchids", modelID: "orchids-unavailable-model"},
	}

	for _, tc := range testCases {
		t.Run(tc.channel, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()

			ctx := context.Background()
			clearModelsForChannel(t, ctx, s, tc.channel)
			record := &store.Model{
				Channel:   tc.channel,
				ModelID:   tc.modelID,
				Name:      tc.modelID,
				Status:    store.ModelStatusAvailable,
				Verified:  true,
				IsDefault: true,
				SortOrder: 999,
			}
			if err := s.CreateModel(ctx, record); err != nil {
				t.Fatalf("CreateModel() error = %v", err)
			}

			result, err := applyModelRefresh(ctx, s, tc.channel, "test", nil)
			if err != nil {
				t.Fatalf("applyModelRefresh() error = %v", err)
			}
			if result.Deleted != 0 {
				t.Fatalf("Deleted=%d want 0", result.Deleted)
			}
			if result.Offline != 0 {
				t.Fatalf("Offline=%d want 0", result.Offline)
			}
			if len(result.OfflineModelIDs) != 0 {
				t.Fatalf("OfflineModelIDs=%v want empty", result.OfflineModelIDs)
			}

			model, err := s.GetModelByChannelAndModelID(ctx, tc.channel, tc.modelID)
			if err != nil {
				t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
			}
			if model == nil {
				t.Fatalf("expected %s to remain offline", tc.modelID)
			}
			if model.Status != store.ModelStatusAvailable {
				t.Fatalf("Status=%q want %q", model.Status, store.ModelStatusAvailable)
			}
			if !model.Verified {
				t.Fatal("Verified=false want true")
			}
			if !model.IsDefault {
				t.Fatal("IsDefault=false want true")
			}
		})
	}
}

func TestApplyModelRefresh_DeletesMissingWarpGraphQLModels(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Warp")
	for _, record := range []*store.Model{
		{Channel: "Warp", ModelID: "claude-4-5-opus", Name: "Old Opus", Status: store.ModelStatusAvailable, Verified: true, IsDefault: true, SortOrder: 0},
		{Channel: "Warp", ModelID: "auto-open", Name: "Auto Open", Status: store.ModelStatusAvailable, Verified: true, SortOrder: 1},
	} {
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel() error = %v", err)
		}
	}

	result, err := applyModelRefresh(ctx, s, "Warp", "warp_graphql_agent_mode_llms", []discoveredModel{
		{ID: "auto-open", Name: "Auto Open", SortOrder: 0},
		{ID: "gpt-5-2-low", Name: "GPT-5.2 Low", SortOrder: 1},
	})
	if err != nil {
		t.Fatalf("applyModelRefresh() error = %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted=%d want 1", result.Deleted)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "Warp", "claude-4-5-opus"); err == nil {
		t.Fatal("expected old model to be deleted")
	}
	model, err := s.GetModelByChannelAndModelID(ctx, "Warp", "auto-open")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(auto-open) error = %v", err)
	}
	if !model.IsDefault {
		t.Fatal("auto-open IsDefault=false want true")
	}
}

func TestApplyModelRefresh_PreservesMissingWarpWorkspaceFallbackModels(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Warp")
	for _, record := range []*store.Model{
		{Channel: "Warp", ModelID: "claude-4-5-opus", Name: "Old Opus", Status: store.ModelStatusAvailable, Verified: true, IsDefault: false, SortOrder: 0},
		{Channel: "Warp", ModelID: "auto-open", Name: "Auto Open", Status: store.ModelStatusAvailable, Verified: true, IsDefault: true, SortOrder: 1},
	} {
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel() error = %v", err)
		}
	}

	result, err := applyModelRefresh(ctx, s, "Warp", "warp_graphql_workspace_available_llms_fallback", []discoveredModel{
		{ID: "auto-open", Name: "Auto Open", SortOrder: 0},
	})
	if err != nil {
		t.Fatalf("applyModelRefresh() error = %v", err)
	}
	if result.Deleted != 0 {
		t.Fatalf("Deleted=%d want 0", result.Deleted)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "Warp", "claude-4-5-opus"); err != nil {
		t.Fatal("expected workspace fallback refresh to preserve missing model")
	}
}

func TestSaveWarpAccountModelChoices(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	saveWarpAccountModelChoices(ctx, s, []warpAccountDiscovery{
		{
			id: 1,
			ok: true,
			choices: []warp.ModelChoice{
				{ID: "gpt-5.2-medium"},
				{ID: "claude-opus-4-6"},
			},
		},
		{
			id: 2,
			ok: false,
			choices: []warp.ModelChoice{
				{ID: "gemini-3-pro"},
			},
		},
	})

	choices, err := warp.LoadAccountModelChoices(ctx, s)
	if err != nil {
		t.Fatalf("LoadAccountModelChoices() error = %v", err)
	}
	if choices == nil {
		t.Fatal("expected cached choices")
	}
	if !warp.AccountSupportsModel(choices, 1, "claude-4.6-opus") {
		t.Fatal("expected account 1 to support normalized Claude model")
	}
	if warp.AccountSupportsModel(choices, 1, "gemini-3-pro") {
		t.Fatal("expected account 1 not to support uncached Gemini model")
	}
}

func TestApplyModelRefresh_PreservesExistingModelSettings(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Warp")
	record := &store.Model{
		Channel:   "Warp",
		ModelID:   "claude-4-5-sonnet",
		Name:      "Old Name",
		Status:    store.ModelStatusOffline,
		Verified:  false,
		IsDefault: false,
		SortOrder: 999,
	}
	if err := s.CreateModel(ctx, record); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	candidates := []discoveredModel{{ID: "claude-4-5-sonnet", Name: "Claude 4.5 Sonnet (Warp)", SortOrder: 0}}
	result, err := applyModelRefresh(ctx, s, "Warp", "test", candidates)
	if err != nil {
		t.Fatalf("applyModelRefresh() error = %v", err)
	}
	if result.Deleted != 0 {
		t.Fatalf("Deleted=%d want 0", result.Deleted)
	}
	if result.Updated != 0 {
		t.Fatalf("Updated=%d want 0", result.Updated)
	}

	model, err := s.GetModelByChannelAndModelID(ctx, "Warp", "claude-4-5-sonnet")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if model == nil {
		t.Fatal("expected model to remain in store")
	}
	if model.Status != store.ModelStatusOffline {
		t.Fatalf("Status=%q want %q", model.Status, store.ModelStatusOffline)
	}
	if model.Verified {
		t.Fatal("Verified=true want false")
	}
	if model.Name != "Old Name" {
		t.Fatalf("Name=%q want %q", model.Name, "Old Name")
	}
	if model.SortOrder != 999 {
		t.Fatalf("SortOrder=%d want 999", model.SortOrder)
	}
}

func TestRefreshModelRequestConfig_ClampsOrchidsTimeout(t *testing.T) {
	cfg := refreshModelRequestConfig(&config.Config{RequestTimeout: 30}, "Orchids")
	if cfg.RequestTimeout != 10 {
		t.Fatalf("RequestTimeout=%d want 10", cfg.RequestTimeout)
	}
}

func setupModelRefreshStore(t *testing.T) (*store.Store, func()) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "model_refresh_test:",
	})
	if err != nil {
		mini.Close()
		t.Fatalf("store.New() error = %v", err)
	}

	return s, func() {
		_ = s.Close()
		mini.Close()
	}
}

func clearModelsForChannel(t *testing.T, ctx context.Context, s *store.Store, channel string) {
	t.Helper()

	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(model.Channel, channel) {
			continue
		}
		if err := s.DeleteModel(ctx, model.ID); err != nil {
			t.Fatalf("DeleteModel(%q) error = %v", model.ID, err)
		}
	}
}
