package grok

import (
	"errors"
	"testing"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

func TestBuildPayloadForAccountUsesCatalogDefaultAndValidation(t *testing.T) {
	acc := &store.Account{GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.7", ReasoningEfforts: []string{"xhigh", "high", "medium", "low"}, DefaultReasoningEffort: "high", SupportsReasoningEffort: true}}}
	payload, err := buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"summary": "auto"}}, acc, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	reasoning := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "high" {
		t.Fatalf("effort=%v", reasoning["effort"])
	}
	_, err = buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "none"}}, acc, "grok-4.7")
	var profileErr *buildReasoningProfileError
	if !errors.As(err, &profileErr) {
		t.Fatalf("err=%v", err)
	}
	payload, err = buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "max"}}, acc, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"].(map[string]interface{})["effort"] != "xhigh" {
		t.Fatalf("payload=%v", payload)
	}
}

func TestBuildPayloadForAccountDoesNotMutateImmutableSource(t *testing.T) {
	source := map[string]interface{}{"reasoning": map[string]interface{}{"effort": "max"}}
	acc := &store.Account{GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.7", ReasoningEfforts: []string{"xhigh"}, SupportsReasoningEffort: true}}}
	if _, err := buildPayloadForAccount(source, acc, "grok-4.7"); err != nil {
		t.Fatal(err)
	}
	if source["reasoning"].(map[string]interface{})["effort"] != "max" {
		t.Fatalf("source mutated: %v", source)
	}
}
