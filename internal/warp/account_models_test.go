package warp

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
)

func TestAccountModelChoices_RoundTripAndSupport(t *testing.T) {
	mini := miniredis.RunT(t)
	defer mini.Close()

	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "warp_account_model_test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	err = SaveAccountModelChoices(ctx, s, &AccountModelChoices{
		Accounts: map[string][]string{
			"1": {"gpt-5.2-medium", "gpt-5-2-medium", "claude-opus-4-6"},
		},
	})
	if err != nil {
		t.Fatalf("SaveAccountModelChoices() error = %v", err)
	}

	choices, err := LoadAccountModelChoices(ctx, s)
	if err != nil {
		t.Fatalf("LoadAccountModelChoices() error = %v", err)
	}
	got := choices.Accounts["1"]
	want := []string{"claude-4-6-opus-high", "gpt-5-2-medium"}
	if len(got) != len(want) {
		t.Fatalf("models=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models=%v want %v", got, want)
		}
	}
	if !AccountSupportsModel(choices, 1, "claude-4.6-opus") {
		t.Fatal("expected account to support normalized claude alias")
	}
	if AccountSupportsModel(choices, 1, "gemini-3-pro") {
		t.Fatal("expected account not to support missing model")
	}
	if !AccountSupportsModel(choices, 2, "gemini-3-pro") {
		t.Fatal("expected missing account cache to fall back open")
	}
	if !AccountSupportsModel(choices, 1, DefaultModel()) {
		t.Fatal("expected default model to remain always selectable")
	}
}

func TestAccountModelUnavailable_TTLAndFilter(t *testing.T) {
	mini := miniredis.RunT(t)
	defer mini.Close()

	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "warp_account_model_unavailable_test:",
	})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	if err := MarkAccountModelUnavailable(ctx, s, 7, "claude-opus-4-6", now); err != nil {
		t.Fatalf("MarkAccountModelUnavailable() error = %v", err)
	}

	if !AccountModelTemporarilyUnavailable(ctx, s, 7, "claude-4.6-opus", now.Add(time.Hour)) {
		t.Fatal("expected model to be temporarily unavailable")
	}

	choices := []ModelChoice{
		{ID: "auto-open", Name: "Auto Open"},
		{ID: "claude-opus-4-6", Name: "Claude Opus"},
		{ID: "gpt-5.2-medium", Name: "GPT"},
	}
	filtered := FilterUnavailableModels(ctx, s, 7, choices, now.Add(time.Hour))
	gotIDs := make([]string, 0, len(filtered))
	for _, choice := range filtered {
		gotIDs = append(gotIDs, choice.ID)
	}
	want := []string{"auto-open", "gpt-5.2-medium"}
	if len(gotIDs) != len(want) {
		t.Fatalf("filtered ids=%v want %v", gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("filtered ids=%v want %v", gotIDs, want)
		}
	}
	if AccountModelTemporarilyUnavailable(ctx, s, 7, "claude-4.6-opus", now.Add(7*time.Hour)) {
		t.Fatal("expected model unavailable cache to expire")
	}
}
