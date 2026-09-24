package modelcatalog

import (
	"reflect"
	"testing"
)

func TestAggregateProfilesKeepsStableOrderAndRichestBudgets(t *testing.T) {
	first := []Profile{{ModelID: "grok-4.7", ReasoningEfforts: []string{"high", "low"}, DefaultReasoningEffort: "high", ContextWindow: 128000}}
	second := []Profile{{ModelID: "GROK-4.7", ReasoningEfforts: []string{"xhigh", "high"}, ContextWindow: 500000, MaxCompletionTokens: 1000000, SupportsBackendSearch: true}, {ModelID: "grok-4.6"}}
	got := Aggregate(first, second)
	if len(got) != 2 || got[0].ModelID != "grok-4.7" || got[1].ModelID != "grok-4.6" {
		t.Fatalf("Aggregate()=%+v", got)
	}
	if !reflect.DeepEqual(got[0].ReasoningEfforts, []string{"high"}) || got[0].ContextWindow != 128000 || got[0].MaxCompletionTokens != 1000000 || got[0].SupportsBackendSearch {
		t.Fatalf("merged profile=%+v", got[0])
	}
	got[0].ReasoningEfforts[0] = "mutated"
	if first[0].ReasoningEfforts[0] != "high" {
		t.Fatal("Aggregate returned a shallow copy")
	}
}
