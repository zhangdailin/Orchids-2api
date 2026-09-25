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
)

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

func TestValidateModelAvailability_WorkBuddyUsesChannelSpecificModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})

	got, err := h.validateModelAvailability(ctx, "claude-opus-5", "workbuddy")
	if err != nil {
		t.Fatalf("validateModelAvailability() error = %v", err)
	}
	if got == nil {
		t.Fatal("validateModelAvailability() returned nil model")
	}
	if got.Channel != "WorkBuddy" {
		t.Fatalf("validateModelAvailability() channel = %q, want %q", got.Channel, "WorkBuddy")
	}
	if got.ModelID != "claude-opus-5" {
		t.Fatalf("validateModelAvailability() model = %q, want %q", got.ModelID, "claude-opus-5")
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

func TestSelectAccountRecord_ClineEnforcesPerAccountCatalog(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	ctx := context.Background()
	first := &store.Account{Name: "cline-a", AccountType: "cline", ClineAccessToken: "a", ClineModelIDs: []string{`{"id":"model-a"}`}, Enabled: true, Weight: 1}
	second := &store.Account{Name: "cline-b", AccountType: "cline", ClineAccessToken: "b", ClineModelIDs: []string{`{"id":"model-b"}`}, Enabled: true, Weight: 1}
	for _, acc := range []*store.Account{first, second} {
		if err := s.CreateAccount(ctx, acc); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := h.selectAccountRecordWithOptions(ctx, "cline", nil, accountSelectionOptions{ModelID: "model-b"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != second.ID {
		t.Fatalf("selected=%d want=%d", selected.ID, second.ID)
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

	mustCreateModel(t, s, "199", "WorkBuddy", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "200", "WorkBuddy", "claude-opus-4.6", store.ModelStatusAvailable)

	got, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "workbuddy")
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

	mustCreateModel(t, s, "199", "WorkBuddy", "claude-opus-4-6", store.ModelStatusOffline)

	mustCreateModel(t, s, "201", "WorkBuddy", "claude-opus-4.6", store.ModelStatusOffline)

	_, err := h.validateModelAvailability(ctx, "claude-opus-4-6", "workbuddy")
	if err == nil {
		t.Fatal("validateModelAvailability() error = nil, want model not available")
	}
	if err.Error() != "model not available" {
		t.Fatalf("validateModelAvailability() error = %q, want %q", err.Error(), "model not available")
	}
}

// A channel may publish models as "<family>-<effort>"; a client that asks for
// the family name plus reasoning_effort must land on the matching catalog entry
// instead of a "model not found" rejection.
func TestResolveEffortModelVariant(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	ctx := context.Background()
	mustCreateModel(t, s, "301", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "302", "WorkBuddy", "claude-opus-5-medium", store.ModelStatusAvailable)
	mustCreateModel(t, s, "303", "WorkBuddy", "claude-opus-5-high", store.ModelStatusAvailable)

	cases := []struct {
		name     string
		model    string
		effort   string
		channel  string
		expected string
	}{
		{"requested effort wins", "claude-opus-5", "low", "workbuddy", "claude-opus-5-low"},
		{"defaults to medium", "claude-opus-5", "", "workbuddy", "claude-opus-5-medium"},
		{"unknown effort falls back", "claude-opus-5", "turbo", "workbuddy", "claude-opus-5-medium"},
		{"exact hit wins", "claude-opus-5-high", "low", "workbuddy", "claude-opus-5-high"},
		{"unknown family is untouched", "claude-opus-9-unknown", "low", "workbuddy", "claude-opus-9-unknown"},
		{"empty model is untouched", "", "low", "workbuddy", ""},
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

	mustCreateModel(t, s, "310", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)

	got := h.resolveEffortModelVariant(context.Background(), "CLAUDE-OPUS-5", "LOW", "workbuddy")
	if got != "claude-opus-5-low" {
		t.Fatalf("resolved = %q, want the lower-case catalog id", got)
	}
}

func TestHandleMessages_ResolvesBareModelToEffortVariant(t *testing.T) {
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
		Name:         "workbuddy-1",
		AccountType:  "workbuddy",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	mustCreateModel(t, s, "401", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "402", "WorkBuddy", "claude-opus-5-medium", store.ModelStatusAvailable)

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, lb)
	client := &fakePayloadClient{}
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient { return client })

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false,"reasoning_effort":"low"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", strings.NewReader(body))
	h.HandleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(client.calls))
	}
	if client.calls[0].Model != "claude-opus-5-low" {
		t.Fatalf("upstream model = %q, want the effort variant claude-opus-5-low", client.calls[0].Model)
	}
	// The client-stated effort must reach the provider request so channels
	// whose wire contract carries it (qoder/workbuddy/cline) can forward
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
		Name:         "workbuddy-1",
		AccountType:  "workbuddy",
		RefreshToken: "rt",
		Enabled:      true,
		Weight:       1,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	for index, modelID := range models {
		mustCreateModel(t, s, strconv.Itoa(600+index), "WorkBuddy", modelID, store.ModelStatusAvailable)
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
			`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false,"output_config":{"effort":"high"}}`,
			"claude-opus-5-high",
		},
		"thinking": {
			`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false,"thinking":{"effort":"low"}}`,
			"claude-opus-5-low",
		},
		"thinking budget": {
			`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"stream":false,"thinking":{"budget_tokens":1024}}`,
			"claude-opus-5-low",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, client := newEffortResolutionHandler(t, "claude-opus-5-low", "claude-opus-5-medium", "claude-opus-5-high")
			rec := httptest.NewRecorder()
			h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", strings.NewReader(tc.body)))
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
// "claude-opus-5-low" with reasoning_effort "high" was silently served as
// "claude-opus-5-medium" — an effort the client never asked for.
func TestResolveEffortModelVariant_DoesNotResuffixAnEffortVariant(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "320", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "321", "WorkBuddy", "claude-opus-5-medium", store.ModelStatusAvailable)
	mustCreateModel(t, s, "322", "WorkBuddy", "claude-opus-5-high", store.ModelStatusAvailable)

	cases := []struct {
		name   string
		model  string
		effort string
		want   string
	}{
		{"known variant keeps its own effort", "claude-opus-5-low", "high", "claude-opus-5-low"},
		{"known variant ignores a conflicting default", "claude-opus-5-high", "", "claude-opus-5-high"},
		{"unknown variant under a known family is unchanged", "claude-opus-5-unknown", "low", "claude-opus-5-unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.resolveEffortModelVariant(context.Background(), tc.model, tc.effort, "workbuddy"); got != tc.want {
				t.Fatalf("resolveEffortModelVariant(%q, %q) = %q, want %q", tc.model, tc.effort, got, tc.want)
			}
		})
	}
}

// On the unified prefix the path names no channel, so the channel must come from
// the model. A path-only answer is what made count_tokens estimate every /v1
// request with the generic profile while the completion ran on another channel.
func TestModelChannelFallsBackToTheModelOnTheUnifiedPrefix(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	mustCreateModel(t, s, "330", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "331", "WorkBuddy", "hy3", store.ModelStatusAvailable)

	cases := []struct {
		path  string
		model string
		want  string
	}{
		{"/cline/v1/messages/count_tokens", "anything", "cline"},
		{"/v1/messages/count_tokens", "claude-opus-5-low", "WorkBuddy"},
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

	mustCreateModel(t, s, "340", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)
	mustCreateModel(t, s, "341", "WorkBuddy", "claude-opus-5-medium", store.ModelStatusAvailable)

	channel, err := h.LookupChannelForModel(context.Background(), "claude-opus-5")
	if err != nil {
		t.Fatalf("LookupChannelForModel() error = %v", err)
	}
	if channel != "WorkBuddy" {
		t.Fatalf("family channel = %q, want WorkBuddy", channel)
	}
	if got := h.ChannelForModel(context.Background(), "claude-opus-5"); got != "WorkBuddy" {
		t.Fatalf("ChannelForModel(family) = %q, want WorkBuddy", got)
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

	mustCreateModel(t, s, "350", "WorkBuddy", "claude-opus-5-low", store.ModelStatusAvailable)

	ctx, _ := middleware.RequestModelHint(context.Background())
	ctx = middleware.WithRequestModel(ctx, "claude-opus-5-low")
	if got := h.ChannelForModel(ctx, "claude-opus-5"); got != "WorkBuddy" {
		t.Fatalf("ChannelForModel with hint = %q, want WorkBuddy", got)
	}
}
