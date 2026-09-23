package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/warp"
)

// publicListEntry is the shape a client reads. The window fields are the point
// of these tests: a client that cannot see them budgets against its own default.
type publicListEntry struct {
	ID              string `json:"id"`
	ContextLength   int    `json:"context_length"`
	MaxInputTokens  int    `json:"max_input_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

func fetchPublicModels(t *testing.T, h *Handler, path string) map[string]publicListEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []publicListEntry `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := make(map[string]publicListEntry, len(payload.Data))
	for _, entry := range payload.Data {
		out[entry.ID] = entry
	}
	return out
}

// The Qoder catalog declares max_input_tokens per model and the request path
// already forwards it. The model list has to publish the same number, otherwise
// a 1M-token model reads as the client's 262144 default.
func TestPublicModelsPublishQoderContextWindow(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "qoder-1", "qoder")
	acc.QoderModelIDs = []string{
		`{"key":"ultimate","name":"Ultimate","display_name":"Ultimate","max_input_tokens":1000000}`,
		`{"key":"qfmodel","name":"Qwen3.8-Flash","display_name":"Qwen3.8-Flash","max_input_tokens":180000}`,
	}
	if err := s.UpdateAccount(context.Background(), acc); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	publishModel(t, s,
		&store.Model{Channel: "qoder", ModelID: "ultimate"},
		&store.Model{Channel: "qoder", ModelID: "qwen3.8-flash"},
	)

	entries := fetchPublicModels(t, h, "/qoder/v1/models")
	if got := entries["ultimate"].ContextLength; got != 1000000 {
		t.Fatalf("ultimate context_length = %d, want 1000000", got)
	}
	if got := entries["ultimate"].MaxInputTokens; got != 1000000 {
		t.Fatalf("ultimate max_input_tokens = %d, want 1000000", got)
	}
	if got := entries["qwen3.8-flash"].ContextLength; got != 180000 {
		t.Fatalf("qwen3.8-flash context_length = %d, want 180000", got)
	}
}

// A window that was never observed must be absent, not zero: a client that reads
// zero would treat the model as unable to hold anything.
func TestPublicModelsOmitUnobservedContextWindow(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s, &store.Model{Channel: "puter", ModelID: "gpt-5-nano"})

	entries := fetchPublicModels(t, h, "/puter/v1/models")
	entry, ok := entries["gpt-5-nano"]
	if !ok {
		t.Fatalf("model missing from the list: %#v", entries)
	}
	if entry.ContextLength != 0 || entry.MaxInputTokens != 0 {
		t.Fatalf("unobserved window must be omitted, got %+v", entry)
	}

	// Re-encode to prove the field is absent rather than present-and-zero.
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com/puter/v1/models", nil))
	if body := rec.Body.String(); containsJSONField(body, "context_length") {
		t.Fatalf("context_length must not be serialized when unobserved: %s", body)
	}
}

// Warp publishes the window with every model choice. It is stored with the
// discovery cache, and the model list has to report it.
func TestPublicModelsPublishWarpContextWindow(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "warp-1", "warp")
	choices := &warp.AccountModelChoices{
		Accounts: map[string][]string{accountKeyForTest(acc.ID): {"gpt-5-6-sol-medium"}},
		ContextWindows: map[string]warp.ModelContextWindow{
			"gpt-5-6-sol-medium": {Min: 1024, Max: 1000000, Default: 128000},
		},
	}
	if err := warp.SaveAccountModelChoices(context.Background(), s, choices); err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}
	publishModel(t, s, &store.Model{Channel: "warp", ModelID: "gpt-5-6-sol-medium"})

	entries := fetchPublicModels(t, h, "/warp/v1/models")
	if got := entries["gpt-5-6-sol-medium"].ContextLength; got != 1000000 {
		t.Fatalf("warp context_length = %d, want 1000000", got)
	}
}

func TestPublicModelsIgnoreDisabledWarpContextSnapshot(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	ctx := context.Background()
	active := createEnabledTestAccount(t, s, "warp-active", "warp")
	disabled := createEnabledTestAccount(t, s, "warp-disabled", "warp")
	disabled.Enabled = false
	if err := s.UpdateAccount(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	if err := warp.UpsertAccountModelDiscoveries(ctx, s,
		warp.AccountModelDiscovery{AccountID: active.ID, Choices: []warp.ModelChoice{{ID: "active-model", ContextWindow: warp.ModelContextWindow{Max: 200000}}}},
		warp.AccountModelDiscovery{AccountID: disabled.ID, Choices: []warp.ModelChoice{{ID: "disabled-model", ContextWindow: warp.ModelContextWindow{Max: 900000}}}},
	); err != nil {
		t.Fatal(err)
	}
	publishModel(t, s,
		&store.Model{Channel: "warp", ModelID: "active-model"},
		&store.Model{Channel: "warp", ModelID: "disabled-model"},
	)

	entries := fetchPublicModels(t, h, "/warp/v1/models")
	if got := entries["active-model"].ContextLength; got != 200000 {
		t.Fatalf("active context_length=%d want 200000", got)
	}
	if _, visible := entries["disabled-model"]; visible {
		t.Fatalf("disabled account model remained visible: %+v", entries["disabled-model"])
	}
}

// WorkBuddy is the channel whose long sessions were measured past 400k tokens.
// Its catalog carries maxInputTokens/maxOutputTokens and both must survive the
// account snapshot.
func TestPublicModelsPublishWorkBuddyContextWindow(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	acc := createEnabledTestAccount(t, s, "wb-1", "workbuddy")
	acc.WorkBuddyModelIDs = []string{
		`{"id":"wb-model","name":"WB Model","max_input_tokens":256000,"max_output_tokens":32000}`,
		// A bare id written by an older build still has to resolve, it just has
		// no window to report.
		"legacy-model",
	}
	if err := s.UpdateAccount(context.Background(), acc); err != nil {
		t.Fatalf("UpdateAccount() error = %v", err)
	}

	publishModel(t, s,
		&store.Model{Channel: "workbuddy", ModelID: "wb-model"},
		&store.Model{Channel: "workbuddy", ModelID: "legacy-model"},
	)

	entries := fetchPublicModels(t, h, "/workbuddy/v1/models")
	if got := entries["wb-model"].ContextLength; got != 256000 {
		t.Fatalf("wb-model context_length = %d, want 256000", got)
	}
	if got := entries["wb-model"].MaxOutputTokens; got != 32000 {
		t.Fatalf("wb-model max_output_tokens = %d, want 32000", got)
	}
	if got := entries["legacy-model"].ContextLength; got != 0 {
		t.Fatalf("legacy-model context_length = %d, want omitted", got)
	}
}

// The Grok window is not account-scoped; it comes from the same table the Codex
// catalog publishes, so both surfaces agree.
func TestPublicModelsPublishGrokContextWindow(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	publishModel(t, s, &store.Model{Channel: "grok", ModelID: "grok-4.3", Verified: true})

	entries := fetchPublicModels(t, h, "/grok/v1/models")
	if got := entries["grok-4.3"].ContextLength; got != 1000000 {
		t.Fatalf("grok-4.3 context_length = %d, want 1000000", got)
	}
}

// The Codex catalog used to fall back to a Grok-shaped 128k default for every
// non-Grok model, which reported a 1M-token model as eight times smaller. An
// observed window must win over that default.
func TestCodexCatalogPrefersObservedContextWindow(t *testing.T) {
	observed := textModel("qwen3.8-max")
	observed.ContextLength = 1000000
	unobserved := textModel("qwen3.8-flash")

	catalog := newCodexModelCatalog([]PublicModelResponse{observed, unobserved})

	if got := codexEntryFor(t, catalog, "qwen3.8-max").ContextWindow; got != 1000000 {
		t.Fatalf("observed window ignored: context_window = %d, want 1000000", got)
	}
	// Nothing was observed for this one, so the historical default still applies
	// and no wrong number is invented.
	if got := codexEntryFor(t, catalog, "qwen3.8-flash").ContextWindow; got != 128000 {
		t.Fatalf("unobserved model context_window = %d, want the 128000 default", got)
	}
}

func accountKeyForTest(id int64) string {
	return strconv.FormatInt(id, 10)
}

func containsJSONField(body, field string) bool {
	return len(body) > 0 && json.Valid([]byte(body)) && jsonContainsKey([]byte(body), field)
}

func jsonContainsKey(body []byte, field string) bool {
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false
	}
	return deepHasKey(decoded, field)
}

func deepHasKey(value interface{}, field string) bool {
	switch typed := value.(type) {
	case map[string]interface{}:
		if _, ok := typed[field]; ok {
			return true
		}
		for _, item := range typed {
			if deepHasKey(item, field) {
				return true
			}
		}
	case []interface{}:
		for _, item := range typed {
			if deepHasKey(item, field) {
				return true
			}
		}
	}
	return false
}
