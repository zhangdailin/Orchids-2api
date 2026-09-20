package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestPublicModelResponseUsesRouteCreatedAt(t *testing.T) {
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	entry := publicModelResponse("grok-4.6", "grok", created)
	if entry.Created != created.Unix() {
		t.Fatalf("created = %d, want %d", entry.Created, created.Unix())
	}
	// A row stored before the field existed keeps the legacy placeholder.
	legacy := publicModelResponse("grok-4.6", "grok", time.Time{})
	if legacy.Created != legacyModelCreated {
		t.Fatalf("legacy created = %d, want %d", legacy.Created, legacyModelCreated)
	}
}

// grok2api publishes bare model names: the console/ and build/ qualifiers are
// routing details, and two routes that differ only by plane are the same public
// model. Resolution keeps accepting both spellings.
func TestPublicModelsPublishExternalIDs(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "console/grok-4.3"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "console/grok-4.6"},
	)

	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := map[string]int{}
	for _, entry := range payload.Data {
		seen[entry.ID]++
	}
	for _, want := range []string{"grok-4.3", "grok-4.5", "grok-4.6"} {
		if seen[want] == 0 {
			t.Fatalf("external model %q missing from the public list: %#v", want, seen)
		}
	}
	for id := range seen {
		if strings.Contains(id, "/") && !strings.HasPrefix(id, "warp-") {
			t.Fatalf("a provider-qualified ID was published: %q", id)
		}
	}
	if seen["grok-4.6"] != 1 {
		t.Fatalf("routes differing only by plane must collapse to one public entry: %#v", seen)
	}
}
