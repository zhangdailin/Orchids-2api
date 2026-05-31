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
	want := []string{"claude-opus-4-6", "gpt-5-2-medium", "gpt-5.2-medium"}
	if len(got) != len(want) {
		t.Fatalf("models=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models=%v want %v", got, want)
		}
	}
	if AccountSupportsModel(choices, 1, "claude-4.6-opus") {
		t.Fatal("expected account to reject missing claude alias")
	}
	if AccountSupportsModel(choices, 1, "gemini-3-pro") {
		t.Fatal("expected account not to support missing model")
	}
	if !ChoicesSupportModel(choices, "gpt-5.2-medium") {
		t.Fatal("expected choices to support cached model")
	}
	if ChoicesSupportModel(choices, "gemini-3-pro") {
		t.Fatal("expected choices not to support model missing from every account")
	}
	if !AccountSupportsModel(choices, 2, "gemini-3-pro") {
		t.Fatal("expected missing account cache to fall back open")
	}
	if AccountSupportsModel(choices, 1, DefaultModel()) {
		t.Fatal("expected default model to require explicit account support when choices are cached")
	}
	exhausted := &store.Account{
		ID:                   1,
		AccountType:          "warp",
		WarpMonthlyLimit:     1500,
		WarpMonthlyRemaining: 0,
		WarpBonusRemaining:   0,
	}
	if !AccountSupportsModelForAccount(choices, exhausted, DefaultModel()) {
		t.Fatal("expected exhausted account to support free-only default model")
	}
	if AccountSupportsModelForAccount(choices, exhausted, "gpt-5.2-medium") {
		t.Fatal("expected exhausted account to reject paid model despite cached paid pool")
	}
	if AccountSupportsModelForAccount(nil, exhausted, "gpt-5.2-medium") {
		t.Fatal("expected exhausted account to reject paid model even when model choices cache is unavailable")
	}
	free := &store.Account{
		ID:                   1,
		AccountType:          "warp",
		Subscription:         "free",
		WarpMonthlyLimit:     60,
		WarpMonthlyRemaining: 58,
	}
	if !AccountSupportsModelForAccount(choices, free, DefaultModel()) {
		t.Fatal("expected free account to support free-only default model")
	}
	if AccountSupportsModelForAccount(choices, free, "gpt-5.2-medium") {
		t.Fatal("expected free account to reject paid model despite cached paid pool")
	}
	probedChoices := &AccountModelChoices{
		Accounts: map[string][]string{"1": {DefaultModel(), "gpt-5-2-low"}},
		Sources:  map[string]string{"1": "free_probe"},
	}
	if !AccountSupportsModelForAccount(probedChoices, free, "gpt-5-2-low") {
		t.Fatal("expected free account to support model confirmed by free probe")
	}
}
