package channel

import "testing"

func TestRegistryInvariantsAndPathParsing(t *testing.T) {
	seenID, seenLabel, seenPrefix := map[ID]bool{}, map[string]bool{}, map[string]bool{}
	defaults := 0
	for _, definition := range All() {
		if seenID[definition.ID] || seenLabel[definition.Label] || seenPrefix[definition.APIPrefix] {
			t.Fatalf("duplicate definition: %+v", definition)
		}
		seenID[definition.ID], seenLabel[definition.Label], seenPrefix[definition.APIPrefix] = true, true, true
		if definition.Default {
			defaults++
		}
		if id, ok := Parse(definition.Label); !ok || id != definition.ID {
			t.Fatalf("label parse failed: %+v", definition)
		}
		if id, ok := FromPath(definition.APIPrefix + "/models"); !ok || id != definition.ID {
			t.Fatalf("path parse failed: %+v", definition)
		}
		if id, model, ok := TrimModelPath(definition.APIPrefix + "/models/example"); !ok || id != definition.ID || model != "example" {
			t.Fatalf("model path parse failed: %+v", definition)
		}
	}
	if defaults != 1 {
		t.Fatalf("defaults=%d want 1", defaults)
	}
}
