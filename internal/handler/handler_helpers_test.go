package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

func TestResolveWorkdir_NoSessionFallbackWithoutExplicitConversation(t *testing.T) {
	ss := NewMemorySessionStore(30*time.Minute, 100)
	ss.SetWorkdir(context.TODO(), "k1", "/stale/workdir")

	h := &Handler{
		sessionStore: ss,
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.com/warp/v1/messages", nil)
	req := ClaudeRequest{}

	got, prev, changed := h.resolveWorkdir(r, req, "k1")
	if got != "" {
		t.Fatalf("expected empty workdir, got %q", got)
	}
	if prev != "/stale/workdir" {
		t.Fatalf("expected prev workdir retained, got %q", prev)
	}
	if changed {
		t.Fatalf("expected changed=false when no new workdir")
	}
}

func setupModelValidationHandler(t *testing.T) (*Handler, *store.Store, *miniredis.Miniredis) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(nil, lb)
	return h, s, mini
}

// publishModel inserts a model row the way an upstream refresh would.
//
// The store no longer starts with a compiled-in catalog, so a test that needs a
// model to exist publishes it. Records default to verified/available with the
// discovery origin, which is what a catalog read produces.
func publishModel(t *testing.T, s *store.Store, records ...*store.Model) {
	t.Helper()
	ctx := context.Background()
	for _, record := range records {
		if record == nil {
			continue
		}
		if record.Channel == "" {
			record.Channel = "Grok"
		}
		if record.Name == "" {
			record.Name = record.ModelID
		}
		if record.Status == "" {
			record.Status = store.ModelStatusAvailable
		}
		if record.Origin == "" {
			record.Origin = "discovery"
		}
		record.Verified = true
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel(%s/%s) error = %v", record.Channel, record.ModelID, err)
		}
	}
}

func TestValidateModelAvailability_PuterUsesChannelSpecificModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	publishModel(t, s, &store.Model{Channel: "Puter", ModelID: "claude-opus-5"})

	got, err := h.validateModelAvailability(ctx, "claude-opus-5", "puter")
	if err != nil {
		t.Fatalf("validateModelAvailability() error = %v", err)
	}
	if got == nil {
		t.Fatal("validateModelAvailability() returned nil model")
	}
	if got.Channel != "Puter" {
		t.Fatalf("validateModelAvailability() channel = %q, want %q", got.Channel, "Puter")
	}
	if got.ModelID != "claude-opus-5" {
		t.Fatalf("validateModelAvailability() model = %q, want %q", got.ModelID, "claude-opus-5")
	}
}

func TestSelectAccountRecord_WarpRejectsModelOutsideCurrentPool(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:  "warp",
		RefreshToken: "warp-free-token",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := warp.SaveAccountModelChoices(ctx, s, &warp.AccountModelChoices{Accounts: map[string][]string{"1": {"auto-open"}}}); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}

	_, err := h.selectAccountRecordWithOptions(ctx, "warp", nil, accountSelectionOptions{ModelID: "gpt-5-2-medium"})
	if err == nil {
		t.Fatal("selectAccountRecord() error = nil, want unavailable model error")
	}
	if !strings.Contains(err.Error(), "not available in the current Warp account pool") {
		t.Fatalf("selectAccountRecord() error = %q", err.Error())
	}
}

func TestSelectAccountRecord_WarpContinuationPinsIssuingAccount(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	for _, name := range []string{"warp-one", "warp-two"} {
		if err := s.CreateAccount(ctx, &store.Account{
			Name:                 name,
			AccountType:          "warp",
			RefreshToken:         name + "-token",
			Subscription:         "build/business",
			WarpMonthlyLimit:     1500,
			WarpMonthlyRemaining: 100,
			Enabled:              true,
			Weight:               1,
		}); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", name, err)
		}
	}

	account, err := h.selectAccountRecordWithOptions(ctx, "warp", nil, accountSelectionOptions{
		ModelID:            "auto-open",
		PreferredAccountID: 2,
	})
	if err != nil {
		t.Fatalf("select pinned account error = %v", err)
	}
	if account == nil || account.ID != 2 {
		t.Fatalf("selected account=%v want id=2", account)
	}
}

// TestSelectAccountRecord_WorkBuddyParksModelNotAccount is the routing half of
// the reported behaviour: a paid-model refusal cools down that model only, so the
// account keeps serving WorkBuddy's free models. Before this rule the refusal was
// account-scoped (or the whole pool was skipped), which took the free models down
// with the paid one.
func TestSelectAccountRecord_WorkBuddyParksModelNotAccount(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	refused := &store.Account{Name: "wb-paid", AccountType: "workbuddy", WorkBuddyAccessToken: "paid-token", Enabled: true, Weight: 1}
	spare := &store.Account{Name: "wb-spare", AccountType: "workbuddy", WorkBuddyAccessToken: "spare-token", Enabled: true, Weight: 1}
	for _, acc := range []*store.Account{refused, spare} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}

	// The paid model was refused with 402 on the first account.
	store.RecordModelCooldown(refused, "paid-model", time.Now().Add(time.Minute))
	if err := s.UpdateAccount(ctx, refused); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	account, err := h.selectAccountRecordWithOptions(ctx, "workbuddy", nil, accountSelectionOptions{ModelID: "paid-model"})
	if err != nil {
		t.Fatalf("selectAccountRecordWithOptions() error = %v", err)
	}
	if account.ID != spare.ID {
		t.Fatalf("selected account %d for the refused model, want the healthy account %d", account.ID, spare.ID)
	}

	// Only the named model is parked: the refused account still serves free models.
	account, err = h.selectAccountRecordWithOptions(ctx, "workbuddy", []int64{spare.ID}, accountSelectionOptions{ModelID: "free-model"})
	if err != nil {
		t.Fatalf("selectAccountRecordWithOptions(free-model) error = %v", err)
	}
	if account.ID != refused.ID {
		t.Fatalf("selected account %d for a free model, want the credit-exhausted account %d to stay in rotation", account.ID, refused.ID)
	}
}

// mustCreateModel inserts a model directly (avoiding reliance on seed data).
func mustCreateModel(t *testing.T, s *store.Store, id string, channel, modelID string, status store.ModelStatus) *store.Model {
	t.Helper()
	m := &store.Model{
		ID:        id,
		Channel:   channel,
		ModelID:   modelID,
		Name:      modelID,
		Status:    status,
		IsDefault: false,
		SortOrder: 0,
	}
	if err := s.UpdateModel(context.Background(), m); err != nil {
		t.Fatalf("UpdateModel(%s) error = %v", modelID, err)
	}
	return m
}

func TestValidateModelAvailability_RejectsOfflineExactMatchEvenWhenAliasExists(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	mustCreateModel(t, s, "199", "Puter", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "200", "Puter", "claude-opus-4.6", store.ModelStatusAvailable)

	got, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "puter")
	if err == nil {
		t.Fatalf("validateModelAvailability() error = nil, got model=%v", got)
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}

func TestValidateModelAvailability_ReturnsOfflineExactMatch(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()

	mustCreateModel(t, s, "199", "Puter", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "201", "Puter", "claude-opus-4.6", store.ModelStatusOffline)

	_, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "puter")
	if err == nil {
		t.Fatal("validateModelAvailability() error = nil, want model not available")
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}

// Warp publishes models as "<family>-<effort>"; a client that asks for the
// family name plus reasoning_effort must land on the matching catalog entry
// instead of a "model not found" rejection.
func TestResolveEffortModelVariant(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	mustCreateModel(t, s, "301", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "302", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)
	mustCreateModel(t, s, "303", "Warp", "gpt-5-6-sol-high", store.ModelStatusAvailable)

	cases := []struct {
		name     string
		model    string
		effort   string
		channel  string
		expected string
	}{
		{"requested effort wins", "gpt-5-6-sol", "low", "warp", "gpt-5-6-sol-low"},
		{"defaults to medium", "gpt-5-6-sol", "", "warp", "gpt-5-6-sol-medium"},
		{"unknown effort falls back", "gpt-5-6-sol", "turbo", "warp", "gpt-5-6-sol-medium"},
		{"exact hit wins", "gpt-5-6-sol-high", "low", "warp", "gpt-5-6-sol-high"},
		{"unknown family is untouched", "gpt-9-unknown", "low", "warp", "gpt-9-unknown"},
		{"empty model is untouched", "", "low", "warp", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.resolveEffortModelVariant(ctx, tc.model, tc.effort, tc.channel); got != tc.expected {
				t.Fatalf("resolveEffortModelVariant(%q, %q, %q) = %q, want %q", tc.model, tc.effort, tc.channel, got, tc.expected)
			}
		})
	}
}

func TestResolveEffortModelVariant_CaseInsensitiveRequest(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "310", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)

	got := h.resolveEffortModelVariant(context.Background(), "GPT-5-6-SOL", "LOW", "warp")
	if got != "gpt-5-6-sol-low" {
		t.Fatalf("resolved = %q, want the lower-case catalog id", got)
	}
}

func TestHandleMessages_WarpResolvesBareModelToEffortVariant(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		Name:         "warp-1",
		AccountType:  "warp",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	mustCreateModel(t, s, "401", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "402", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, lb)
	client := &fakePayloadClient{}
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient { return client })

	body := `{"model":"gpt-5-6-sol","messages":[{"role":"user","content":"hi"}],"stream":false,"reasoning_effort":"low"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/warp/v1/chat/completions", strings.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(client.calls))
	}
	if client.calls[0].Model != "gpt-5-6-sol-low" {
		t.Fatalf("upstream model = %q, want the effort variant gpt-5-6-sol-low", client.calls[0].Model)
	}
	// The client-stated effort must reach the provider request so channels
	// whose wire contract carries it (qoder/workbuddy/puter/cline) can forward
	// the thinking hint instead of silently dropping it.
	if client.calls[0].ReasoningEffort != "low" {
		t.Fatalf("upstream ReasoningEffort = %q, want low", client.calls[0].ReasoningEffort)
	}
}

func TestRequestReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		req  ClaudeRequest
		want string
	}{
		{"openai field", ClaudeRequest{ReasoningEffort: "LOW"}, "low"},
		{"output_config effort", ClaudeRequest{OutputConfig: map[string]interface{}{"effort": "High"}}, "high"},
		{"thinking effort", ClaudeRequest{Thinking: map[string]interface{}{"effort": "medium"}}, "medium"},
		{"openai field wins", ClaudeRequest{ReasoningEffort: "xhigh", OutputConfig: map[string]interface{}{"effort": "low"}}, "xhigh"},
		{"small thinking budget", ClaudeRequest{Thinking: map[string]interface{}{"budget_tokens": float64(2048)}}, "low"},
		{"mid thinking budget", ClaudeRequest{Thinking: map[string]interface{}{"budget_tokens": float64(8192)}}, "medium"},
		{"large thinking budget", ClaudeRequest{Thinking: map[string]interface{}{"budget_tokens": float64(32000)}}, "high"},
		{"thinking disabled", ClaudeRequest{Thinking: map[string]interface{}{"type": "disabled"}}, ""},
		{"no hint", ClaudeRequest{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestReasoningEffort(tc.req); got != tc.want {
				t.Fatalf("requestReasoningEffort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func newEffortResolutionHandler(t *testing.T, models ...string) (*Handler, *fakePayloadClient) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	if err := s.CreateAccount(context.Background(), &store.Account{
		Name:         "warp-1",
		AccountType:  "warp",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	for index, modelID := range models {
		mustCreateModel(t, s, strconv.Itoa(600+index), "Warp", modelID, store.ModelStatusAvailable)
	}

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, lb)
	client := &fakePayloadClient{}
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient { return client })
	return h, client
}

func TestHandleMessages_ResolvesEffortFromAnthropicHints(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"output_config": {
			`{"model":"gpt-5-6-sol","messages":[{"role":"user","content":"hi"}],"stream":false,"output_config":{"effort":"high"}}`,
			"gpt-5-6-sol-high",
		},
		"thinking": {
			`{"model":"gpt-5-6-sol","messages":[{"role":"user","content":"hi"}],"stream":false,"thinking":{"effort":"low"}}`,
			"gpt-5-6-sol-low",
		},
		"thinking budget": {
			`{"model":"gpt-5-6-sol","messages":[{"role":"user","content":"hi"}],"stream":false,"thinking":{"budget_tokens":1024}}`,
			"gpt-5-6-sol-low",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, client := newEffortResolutionHandler(t, "gpt-5-6-sol-low", "gpt-5-6-sol-medium", "gpt-5-6-sol-high")
			rec := httptest.NewRecorder()
			h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/warp/v1/messages", strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.calls) != 1 || client.calls[0].Model != tc.want {
				t.Fatalf("upstream calls = %+v, want model %q", client.calls, tc.want)
			}
		})
	}
}

// A model id that already names an effort variant must never be suffixed again:
// the old fallback appended "-<effort>" and then walked the default order, so
// "gpt-5-6-sol-low" with reasoning_effort "high" was silently served as
// "gpt-5-6-sol-medium" — an effort the client never asked for.
func TestResolveEffortModelVariant_DoesNotResuffixAnEffortVariant(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "320", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "321", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)
	mustCreateModel(t, s, "322", "Warp", "gpt-5-6-sol-high", store.ModelStatusAvailable)

	cases := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{"known variant keeps its own effort", "gpt-5-6-sol-low", "high", "gpt-5-6-sol-low"},
		{"known variant ignores a conflicting default", "gpt-5-6-sol-high", "", "gpt-5-6-sol-high"},
		{"unknown variant under a known family is unchanged", "gpt-5-6-sol-unknown", "low", "gpt-5-6-sol-unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.resolveEffortModelVariant(context.Background(), tc.model, tc.effort, "warp"); got != tc.want {
				t.Fatalf("resolveEffortModelVariant(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
			}
		})
	}
}

// On the unified prefix the path names no channel, so the channel must come from
// the model. A path-only answer is what made count_tokens estimate every /v1
// request with the generic profile while the completion ran on Warp.
func TestModelChannelFallsBackToTheModelOnTheUnifiedPrefix(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "330", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "331", "WorkBuddy", "hy3", store.ModelStatusAvailable)

	cases := []struct {
		path  string
		model string
		want  string
	}{
		{"/warp/v1/messages/count_tokens", "anything", "warp"},
		{"/v1/messages/count_tokens", "gpt-5-6-sol-low", "Warp"},
		{"/v1/messages/count_tokens", "hy3", "WorkBuddy"},
		{"/v1/messages/count_tokens", "no-such-model", ""},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "http://x"+tc.path, nil)
		if got := h.ModelChannel(r, tc.model); got != tc.want {
			t.Fatalf("ModelChannel(%q, %q) = %q, want %q", tc.path, tc.model, got, tc.want)
		}
	}
}

// The catalog advertises the family slug, so the by-id endpoint has to resolve
// that family onto a variant the store actually has. Answering 404 here is what
// pushes a Codex client back to guessing suffixes.
func TestChannelLookupResolvesAnEffortFamilyName(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "340", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "341", "Warp", "gpt-5-6-sol-medium", store.ModelStatusAvailable)

	channel, err := h.LookupChannelForModel(context.Background(), "gpt-5-6-sol")
	if err != nil {
		t.Fatalf("LookupChannelForModel() error = %v", err)
	}
	if channel != "Warp" {
		t.Fatalf("family channel = %q, want Warp", channel)
	}
	if got := h.ChannelForModel(context.Background(), "gpt-5-6-sol"); got != "Warp" {
		t.Fatalf("ChannelForModel(family) = %q, want Warp", got)
	}
	if got := h.ChannelForModel(context.Background(), "gpt-9-unknown"); got != "" {
		t.Fatalf("ChannelForModel(unknown) = %q, want empty", got)
	}
}

// The dispatcher publishes the model it routed on; downstream resolution must
// prefer that single decision over repeating the lookup with a different name.
func TestChannelLookupPrefersThePublishedRequestModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "350", "Warp", "gpt-5-6-sol-low", store.ModelStatusAvailable)

	ctx, _ := middleware.RequestModelHint(context.Background())
	ctx = middleware.WithRequestModel(ctx, "gpt-5-6-sol-low")
	if got := h.ChannelForModel(ctx, "gpt-5-6-sol"); got != "Warp" {
		t.Fatalf("ChannelForModel with hint = %q, want Warp", got)
	}
}
