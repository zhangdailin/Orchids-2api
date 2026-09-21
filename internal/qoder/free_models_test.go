package qoder

import "testing"

func TestFreeModelIDsRequiresExplicitZeroPriceFactor(t *testing.T) {
	ids := []string{
		`{"key":"free-key","name":"Free Model","price_factor":0}`,
		`{"key":"paid-key","name":"Paid Model","price_factor":0.1}`,
		`{"key":"unknown-key","name":"Unknown Model"}`,
		"legacy-key\tLegacy Model",
	}
	for _, name := range []string{"free-key", "Free Model", "FREE MODEL"} {
		if !IsFreeModel(ids, name) {
			t.Fatalf("IsFreeModel(%q)=false, want true", name)
		}
	}
	for _, name := range []string{"paid-key", "unknown-key", "legacy-key", "missing"} {
		if IsFreeModel(ids, name) {
			t.Fatalf("IsFreeModel(%q)=true, want false", name)
		}
	}
}

func TestCatalogSnapshotPreservesExplicitZeroPriceFactor(t *testing.T) {
	zero := 0.0
	catalog := newCatalog([]modelEntry{{Key: "qfmodel", Name: "Qwen3.8-Flash", PriceFactor: &zero}})
	ids := CatalogSnapshot(catalog)
	if len(ids) != 1 || !IsFreeModel(ids, "qwen3.8-flash") {
		t.Fatalf("snapshot=%v did not preserve explicit zero price factor", ids)
	}
}
