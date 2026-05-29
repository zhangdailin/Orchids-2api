package warp

import (
	"context"
	"testing"

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
