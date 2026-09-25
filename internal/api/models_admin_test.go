package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

func seedAdminModels(t *testing.T, s *store.Store, models ...*store.Model) {
	t.Helper()
	for _, m := range models {
		if err := s.CreateModel(context.Background(), m); err != nil {
			t.Fatalf("CreateModel(%s): %v", m.ID, err)
		}
	}
}

// The legacy shape must survive: without a page parameter the endpoint keeps
// returning the bare array the bundled admin UI decodes.
func TestHandleModelsStaysABareArrayWithoutPaging(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	seedAdminModels(t, s, &store.Model{ID: "grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6"})

	rec := httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var bare []store.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &bare); err != nil {
		t.Fatalf("legacy array decode failed: %v (body=%s)", err, rec.Body.String())
	}
	if len(bare) != 1 || bare[0].ModelID != "grok-4.6" {
		t.Fatalf("bare=%+v", bare)
	}
}

// Asking for a page switches to the envelope grok2api's admin client expects.
func TestHandleModelsServesPagedEnvelopeOnRequest(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	seedAdminModels(t, s,
		&store.Model{ID: "grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6"},
		&store.Model{ID: "grok-4.5", Channel: "grok", ModelID: "grok-4.5", Name: "Grok 4.5"},
		&store.Model{ID: "wb-claude", Channel: "workbuddy", ModelID: "claude", Name: "Claude"},
	)

	rec := httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?page=1&pageSize=2", nil))

	var envelope adminModelListEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("envelope decode failed: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Total != 3 || envelope.Page != 1 || envelope.PageSize != 2 || len(envelope.Items) != 2 {
		t.Fatalf("envelope=%+v", envelope)
	}

	rec = httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?page=2&pageSize=2&search=grok", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("filtered envelope decode failed: %v", err)
	}
	if envelope.Total != 2 || len(envelope.Items) != 0 {
		t.Fatalf("search+page envelope=%+v", envelope)
	}

	// pageSize is clamped so one request cannot dump the whole table.
	rec = httptest.NewRecorder()
	a.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models?pageSize=100000", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("clamped envelope decode failed: %v", err)
	}
	if envelope.PageSize != maxAdminModelPageSize {
		t.Fatalf("pageSize=%d want %d", envelope.PageSize, maxAdminModelPageSize)
	}
}

// Groups surface same-name multi-capability routes, which is what the audit
// found missing from the bare model list.
func TestHandleModelGroupsBucketsByEndpointCapabilities(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	seedAdminModels(t, s,
		&store.Model{ID: "grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6",
			Capabilities: []string{"responses", "chat"}},
		&store.Model{ID: "build/grok-4.6", Channel: "grok", ModelID: "grok-4.6", Name: "Grok 4.6 Build",
			Capabilities: []string{"chat", "responses"}},
		&store.Model{ID: "grok-imagine-image", Channel: "grok", ModelID: "grok-imagine-image", Name: "Imagine",
			Capabilities: []string{"image"}},
		&store.Model{ID: "no-caps", Channel: "workbuddy", ModelID: "none", Name: "No caps"},
	)

	rec := httptest.NewRecorder()
	a.HandleModelGroups(rec, httptest.NewRequest(http.MethodGet, "/api/models/groups", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var envelope adminModelGroupEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("group envelope decode failed: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Total != 3 {
		t.Fatalf("groups=%d want 3 (%+v)", envelope.Total, envelope.Items)
	}
	byCaps := map[string]adminModelGroup{}
	for _, group := range envelope.Items {
		byCaps[joinCaps(group.EndpointCapabilities)] = group
	}
	responsesGroup, ok := byCaps["chat,responses"]
	if !ok {
		t.Fatalf("no chat,responses group: %+v", envelope.Items)
	}
	// Capability order in the route must not matter when grouping.
	if len(responsesGroup.Routes) != 2 {
		t.Fatalf("responses group routes=%d want 2", len(responsesGroup.Routes))
	}
	// Key is the joined storage ids of the grouped routes, as in grok2api.
	if responsesGroup.Key == "" || !strings.Contains(responsesGroup.Key, ":") {
		t.Fatalf("group key=%q", responsesGroup.Key)
	}
	// Routes without capabilities cannot be compared, so each stands alone.
	if _, ok := byCaps[""]; !ok {
		t.Fatalf("capability-less route was not its own group: %+v", envelope.Items)
	}

	rec = httptest.NewRecorder()
	a.HandleModelGroups(rec, httptest.NewRequest(http.MethodPost, "/api/models/groups", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d want 405", rec.Code)
	}
}

func joinCaps(caps []string) string {
	out := ""
	for i, cap := range caps {
		if i > 0 {
			out += ","
		}
		out += cap
	}
	return out
}
