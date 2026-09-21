package handler

import (
	"context"
	"testing"

	"orchids-api/internal/store"
)

func TestSelectAccountRecord_ExhaustedPuterOnlyServesCatalogFreeModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	ctx := context.Background()
	acc := &store.Account{AccountType: "puter", ClientCookie: "token", Enabled: true, Weight: 1, StatusCode: store.AccountStatusPuterQuotaExhausted}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	for _, m := range []*store.Model{
		{Channel: "Puter", ModelID: "free-model", Name: "free-model", Status: store.ModelStatusAvailable, BillingTier: "free", BillingSource: "puter_catalog_costs"},
		{Channel: "Puter", ModelID: "paid-model", Name: "paid-model", Status: store.ModelStatusAvailable, BillingTier: "metered", BillingSource: "puter_catalog_costs"},
	} {
		if err := s.CreateModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := h.selectAccountRecordWithOptions(ctx, "puter", nil, accountSelectionOptions{ModelID: "free-model"}); err != nil || got.ID != acc.ID {
		t.Fatalf("free selection got=%v err=%v", got, err)
	}
	if _, err := h.selectAccountRecordWithOptions(ctx, "puter", nil, accountSelectionOptions{ModelID: "paid-model"}); err == nil {
		t.Fatal("paid model unexpectedly selected exhausted Puter account")
	}
}

func TestSelectAccountRecord_ExhaustedWorkBuddyRequiresConfirmedAdvertisedFreeModel(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	ctx := context.Background()
	acc := &store.Account{
		AccountType: "workbuddy", WorkBuddyRefreshToken: "token", Enabled: true, Weight: 1,
		StatusCode:        store.AccountStatusWorkBuddyQuotaExhausted,
		WorkBuddyModelIDs: []string{`{"id":"hy3"}`, `{"id":"gpt-5.6-sol"}`},
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if got, err := h.selectAccountRecordWithOptions(ctx, "workbuddy", nil, accountSelectionOptions{ModelID: "hy3"}); err != nil || got.ID != acc.ID {
		t.Fatalf("free selection got=%v err=%v", got, err)
	}
	for _, model := range []string{"gpt-5.6-sol", "hy4-preview-f"} {
		if _, err := h.selectAccountRecordWithOptions(ctx, "workbuddy", nil, accountSelectionOptions{ModelID: model}); err == nil {
			t.Fatalf("model %q unexpectedly selected exhausted WorkBuddy account", model)
		}
	}
}
func TestSelectAccountRecord_ExhaustedQoderRequiresExplicitAccountFreeFactor(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	defer func() { _ = s.Close(); mini.Close() }()
	ctx := context.Background()
	catalog := []string{`{"key":"qfmodel","name":"Qwen3.8-Flash","price_factor":0}`}
	acc := &store.Account{AccountType: "qoder", QoderRefreshToken: "token", Enabled: true, Weight: 1, StatusCode: store.AccountStatusQoderQuotaExhausted, QoderModelIDs: catalog}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateModel(ctx, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash", Name: "qwen3.8-flash", Status: store.ModelStatusAvailable, BillingTier: "free", BillingSource: "qoder_price_factor"}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.selectAccountRecordWithOptions(ctx, "qoder", nil, accountSelectionOptions{ModelID: "qwen3.8-flash"}); err != nil || got.ID != acc.ID {
		t.Fatalf("free selection got=%v err=%v", got, err)
	}
}
