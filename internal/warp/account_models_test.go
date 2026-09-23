package warp

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
)

func TestUpsertAccountModelDiscoveries_MergesWithoutDroppingLastKnownState(t *testing.T) {
	mini := miniredis.RunT(t)
	defer mini.Close()
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "warp_upsert_test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := SaveAccountModelChoices(ctx, s, &AccountModelChoices{
		Accounts:       map[string][]string{"1": {"old-model"}},
		Sources:        map[string]string{"1": "old-source"},
		ContextWindows: map[string]ModelContextWindow{"old-model": {Max: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAccountModelDiscoveries(ctx, s, AccountModelDiscovery{
		AccountID: 2,
		Source:    "feature_model_choice_all",
		Choices: []ModelChoice{
			{ID: "new-model", ContextWindow: ModelContextWindow{Max: 2000}},
			{ID: "new-model", ContextWindow: ModelContextWindow{Max: 2000}},
		},
		FeatureConfig: AccountFeatureConfig{CliAgentModel: "cli-agent-team-auto"},
	}); err != nil {
		t.Fatalf("UpsertAccountModelDiscoveries() error = %v", err)
	}
	got, err := LoadAccountModelChoices(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts["1"]) != 1 || got.Accounts["1"][0] != "old-model" {
		t.Fatalf("old account was dropped: %+v", got.Accounts)
	}
	if len(got.Accounts["2"]) != 1 || got.Accounts["2"][0] != "new-model" {
		t.Fatalf("new account was not normalized: %+v", got.Accounts)
	}
	if got.ContextWindows["old-model"].Max != 1000 || got.ContextWindows["new-model"].Max != 2000 {
		t.Fatalf("context windows were not merged: %+v", got.ContextWindows)
	}
	if got.FeatureConfigs["2"].CliAgentModel != "cli-agent-team-auto" {
		t.Fatalf("feature config missing: %+v", got.FeatureConfigs)
	}
}

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
		FeatureConfigs: map[string]AccountFeatureConfig{
			"1": {
				BaseModel:             "gpt-5.2-medium",
				CliAgentModel:         "cli-agent-team-auto",
				ComputerUseAgentModel: "computer-use-agent-team-auto",
			},
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

	paid := &store.Account{ID: 1, AccountType: "warp", WarpMonthlyLimit: 1500, WarpMonthlyRemaining: 100}
	cfg := EffectiveAccountFeatureConfig(paid, choices, DefaultModel())
	if cfg.BaseModel != DefaultModel() {
		t.Fatalf("feature base=%q want %q", cfg.BaseModel, DefaultModel())
	}
	if cfg.CliAgentModel != "cli-agent-team-auto" {
		t.Fatalf("cli agent=%q want cli-agent-team-auto", cfg.CliAgentModel)
	}
	if cfg.ComputerUseAgentModel != "computer-use-agent-team-auto" {
		t.Fatalf("computer use agent=%q want computer-use-agent-team-auto", cfg.ComputerUseAgentModel)
	}
}

// TestAccountSupportsModelForRouting pins the policy the request path applies.
//
// Routing answers only "did this account's upstream catalog advertise the model".
// It deliberately does not downgrade an exhausted or free account the way the
// retired AccountSupportsModelForAccount did: Warp decides entitlement at request
// time, so a free account must see the same discovered catalog as a paid one.
// Both halves are asserted so the distinction cannot drift back unnoticed.
func TestAccountSupportsModelForRouting(t *testing.T) {
	choices := &AccountModelChoices{
		Accounts: map[string][]string{"1": {DefaultModel(), "gpt-5.2-medium"}},
	}
	paid := &store.Account{ID: 1, AccountType: "warp", WarpMonthlyLimit: 1500, WarpMonthlyRemaining: 100}

	if !AccountSupportsModelForRouting(choices, paid, "gpt-5.2-medium") {
		t.Fatal("expected an advertised model to be routable")
	}
	if AccountSupportsModelForRouting(choices, paid, "gemini-3-pro") {
		t.Fatal("expected a model outside the catalog to be refused")
	}

	// A missing cache falls back open rather than blocking routing.
	for name, tc := range map[string]struct {
		choices *AccountModelChoices
		acc     *store.Account
	}{
		"no choices":      {nil, paid},
		"empty choices":   {&AccountModelChoices{}, paid},
		"unknown account": {choices, &store.Account{ID: 2, AccountType: "warp"}},
		"no identity":     {choices, &store.Account{}},
		"nil account":     {choices, nil},
	} {
		if !AccountSupportsModelForRouting(tc.choices, tc.acc, "gemini-3-pro") {
			t.Fatalf("%s: expected a fallback-open verdict", name)
		}
	}
	if !AccountSupportsModelForRouting(choices, paid, "") {
		t.Fatal("an empty model name resolves to the default, which the catalog carries")
	}

	// Tier is not part of the routing verdict.
	exhausted := &store.Account{ID: 1, AccountType: "warp", WarpMonthlyLimit: 1500, StatusCode: store.AccountStatusWarpQuotaExhausted}
	free := &store.Account{ID: 1, AccountType: "warp", Subscription: "free", WarpMonthlyLimit: 60}
	for _, acc := range []*store.Account{exhausted, free} {
		if !AccountSupportsModelForRouting(choices, acc, "gpt-5.2-medium") {
			t.Fatalf("account %+v was downgraded for an advertised model", acc)
		}
	}
}
