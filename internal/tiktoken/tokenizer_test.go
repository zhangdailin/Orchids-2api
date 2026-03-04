package tiktoken

import (
	"testing"
)

func TestEstimateTextTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  int
		max  int
	}{
		{
			name: "Pure English",
			text: "This is a test sentence in English.",
			min:  8,
			max:  12,
		},
		{
			name: "Pure Chinese",
			text: "这是一个测试句子。",
			min:  10,
			max:  15,
		},
		{
			name: "Mixed",
			text: "This is a test 这是测试",
			min:  10,
			max:  16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := EstimateTextTokens(tt.text)
			if tokens < tt.min || tokens > tt.max {
				t.Errorf("EstimateTextTokens(%q) = %d, want between %d and %d", tt.text, tokens, tt.min, tt.max)
			}
		})
	}
}
