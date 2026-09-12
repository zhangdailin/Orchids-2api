package api

import "testing"

// TestIsProviderChannel_KeepsInfrastructureOutOfTheMatrix pins the rule the page
// relies on: the "http" catch-all and our own synthetic probes are counted in the
// overview but are not channels an operator can act on, so they must not appear
// as matrix rows or in the channel picker.
func TestIsProviderChannel_KeepsInfrastructureOutOfTheMatrix(t *testing.T) {
	for _, aggregate := range []string{"http", "probe", "HTTP", " probe ", "Probe"} {
		if IsProviderChannel(aggregate) {
			t.Fatalf("%q must not be presented as a provider channel", aggregate)
		}
	}
	for _, channel := range []string{"grok", "warp", "puter", "workbuddy", "GROK"} {
		if !IsProviderChannel(channel) {
			t.Fatalf("%q must remain a provider channel", channel)
		}
	}
	if IsProviderChannel("") {
		t.Fatal("an empty channel is not a provider channel")
	}
}
