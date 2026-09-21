package main

import "testing"

func TestPuterZeroCostRequiresDeclaredCompleteZeroPrices(t *testing.T) {
	tests := []struct {
		name      string
		costs     map[string]interface{}
		inputKey  string
		outputKey string
		known     bool
		free      bool
	}{
		{name: "missing", costs: nil, inputKey: "prompt", outputKey: "completion"},
		{name: "units only", costs: map[string]interface{}{"tokens": float64(1_000_000)}, inputKey: "prompt", outputKey: "completion"},
		{name: "cached zero is not enough", costs: map[string]interface{}{"tokens": float64(1_000_000), "cached_tokens": float64(0)}, inputKey: "prompt", outputKey: "completion"},
		{name: "undeclared keys", costs: map[string]interface{}{"prompt": float64(0), "completion": float64(0)}},
		{name: "zero", costs: map[string]interface{}{"tokens": float64(1_000_000), "prompt": float64(0), "completion": float64(0)}, inputKey: "prompt", outputKey: "completion", known: true, free: true},
		{name: "metered", costs: map[string]interface{}{"tokens": float64(1_000_000), "prompt": float64(7), "completion": float64(14)}, inputKey: "prompt", outputKey: "completion", known: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known, free := puterZeroCost(tt.costs, tt.inputKey, tt.outputKey)
			if known != tt.known || free != tt.free {
				t.Fatalf("puterZeroCost()=(%v,%v), want (%v,%v)", known, free, tt.known, tt.free)
			}
		})
	}
}

func TestNormalizePuterPublicModelDetailsKeepsFreeRoutingMetadata(t *testing.T) {
	items := normalizePuterPublicModelDetails([]puterPublicModelDetails{{
		ID: "openrouter:deepseek/deepseek-v4-flash:free", PuterID: "openrouter:deepseek/deepseek-v4-flash:free",
		Name: "Free Flash", Provider: "openrouter", InputCostKey: "prompt", OutputCostKey: "completion",
		Costs: map[string]interface{}{"tokens": float64(1_000_000), "prompt": float64(0), "completion": float64(0)},
	}})
	if len(items) != 1 || !items[0].Free || !items[0].PricingKnown || items[0].Provider != "openrouter" {
		t.Fatalf("items=%+v", items)
	}
	models := puterChoicesToDiscovered(items)
	if len(models) != 1 || models[0].BillingTier != "free" || models[0].BillingSource != "puter_catalog_costs" || !models[0].Verified {
		t.Fatalf("models=%+v", models)
	}
}
