package handler

import (
	"testing"
	"time"
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
