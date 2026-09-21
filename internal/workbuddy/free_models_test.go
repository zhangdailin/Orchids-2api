package workbuddy

import "testing"

func TestIsFreeModelUsesConfirmedSetOnly(t *testing.T) {
	for _, id := range []string{"hy3", " hy4-preview-f ", "DEEPSEEK-V4.1-FLASH", "glm-5.3-flash"} {
		if !IsFreeModel(id) {
			t.Errorf("IsFreeModel(%q)=false", id)
		}
	}
	for _, id := range []string{"default-model", "gemini-3.5-flash", "glm-5.3", "unknown"} {
		if IsFreeModel(id) {
			t.Errorf("IsFreeModel(%q)=true, must not infer free from its name", id)
		}
	}
}

func TestIsFreeModelInCatalogRequiresAccountAdvertisement(t *testing.T) {
	ids := []string{`{"id":"hy3","name":"HY3"}`, "deepseek-v4.1-flash", `{"id":"gpt-5.6-sol"}`}
	if !IsFreeModelInCatalog(ids, "hy3") || !IsFreeModelInCatalog(ids, "deepseek-v4.1-flash") {
		t.Fatal("confirmed advertised free models should pass")
	}
	if IsFreeModelInCatalog(ids, "hy4-preview-f") {
		t.Fatal("free model missing from this account catalog must not pass")
	}
	if IsFreeModelInCatalog(ids, "gpt-5.6-sol") {
		t.Fatal("advertised paid model must not pass")
	}
}
