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

// TestRegistryIsWorkBuddyDefaultAfterChannelRemoval pins the provider set once
// the fifth channel was retired: WorkBuddy carries the default flag and the
// remaining four channels keep their order, so any accidental re-addition or
// reordering of the registry fails here.
func TestRegistryIsWorkBuddyDefaultAfterChannelRemoval(t *testing.T) {
	if got := Default(); got.ID != WorkBuddy {
		t.Fatalf("Default() = %q, want %q", got.ID, WorkBuddy)
	}
	want := []ID{WorkBuddy, Qoder, Cline, Grok}
	all := All()
	if len(all) != len(want) {
		t.Fatalf("All() has %d channels, want %d", len(all), len(want))
	}
	for i, id := range want {
		if all[i].ID != id {
			t.Fatalf("All()[%d] = %q, want %q", i, all[i].ID, id)
		}
	}
	if len(GenericPrefixes()) != 3 {
		t.Fatalf("GenericPrefixes() = %v, want the three chat-completions channels", GenericPrefixes())
	}
	if len(AllPrefixes()) != len(want) {
		t.Fatalf("AllPrefixes() = %v, want one per channel", AllPrefixes())
	}
}
