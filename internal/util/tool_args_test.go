package util

import "testing"

// TestNormalizeToolInputUnwrapsNestedArguments proves the OpenAI argument
// wrapping is removed before the tool dispatcher sees the input.
func TestNormalizeToolInputUnwrapsNestedArguments(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`{"arguments":"{\"a\":1}"}`: `{"a":1}`,
		`"{\"a\":1}"`:               `{"a":1}`,
		``:                          `{}`,
		`null`:                      `{}`,
		`{"a":1}`:                   `{"a":1}`,
	}
	for input, want := range cases {
		if got := NormalizeToolInput(input); got != want {
			t.Errorf("NormalizeToolInput(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestUnwrapOpenAIArgumentsRejectsOtherShapes pins the fallback: anything that is
// not the argument envelope, or whose inner text is not JSON, reports false so
// the caller keeps the original input.
func TestUnwrapOpenAIArgumentsRejectsOtherShapes(t *testing.T) {
	t.Parallel()

	if got, ok := unwrapOpenAIArguments(`{"arguments":"{\"a\":1}"}`); !ok || got != `{"a":1}` {
		t.Fatalf("unwrapOpenAIArguments() = (%q, %v), want the inner object", got, ok)
	}
	// An argument envelope with no inner text still unwraps to an empty object.
	if got, ok := unwrapOpenAIArguments(`{"arguments":"null"}`); !ok || got != "{}" {
		t.Fatalf("unwrapOpenAIArguments(null text) = (%q, %v), want an empty object", got, ok)
	}
	for name, input := range map[string]string{
		"no arguments key": `{"other":1}`,
		"not an object":    `"plain"`,
		"inner not json":   `{"arguments":"not json"}`,
		"arguments number": `{"arguments":42}`,
		"empty":            ``,
	} {
		if got, ok := unwrapOpenAIArguments(input); ok {
			t.Fatalf("%s: unwrapOpenAIArguments(%q) = (%q, true), want false", name, input, got)
		}
	}
}

// TestNormalizeToolInputDepthStopsAtTheLimit proves the recursion is bounded.
func TestNormalizeToolInputDepthStopsAtTheLimit(t *testing.T) {
	t.Parallel()

	// One layer deeper than the limit is left as-is rather than being unwrapped
	// again, so a nested payload cannot drive unbounded recursion.
	nested := `{"arguments":"{\"arguments\":\"{\\\"a\\\":1}\"}"}`
	if got := normalizeToolInputDepth(nested, 0); got != nested {
		t.Fatalf("depth 0 = %q, want the input unchanged", got)
	}
	if got := NormalizeToolInput(nested); got != `{"a":1}` {
		t.Fatalf("NormalizeToolInput(%q) = %q, want the fully unwrapped object", nested, got)
	}
}
