package tiktoken

import (
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  int
		max  int
	}{
		{
			name: "Empty string",
			text: "",
			min:  0,
			max:  0,
		},
		{
			name: "Simple English word",
			text: "hello",
			min:  1,
			max:  2,
		},
		{
			name: "Short English sentence",
			text: "Hello, world!",
			min:  4,
			max:  6,
		},
		{
			name: "Longer English sentence",
			text: "The quick brown fox jumps over the lazy dog.",
			min:  8,
			max:  15,
		},
		{
			name: "Chinese characters",
			text: "你好世界",
			min:  8,
			max:  10,
		},
		{
			name: "Mixed Chinese and English",
			text: "Hello 你好 World 世界",
			min:  10,
			max:  15,
		},
		{
			name: "Numbers",
			text: "12345 67890",
			min:  2,
			max:  4,
		},
		{
			name: "Special characters",
			text: "!@#$%^&*()",
			min:  10,
			max:  12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := EstimateTokens(tt.text)
			if tokens < tt.min || tokens > tt.max {
				t.Errorf("EstimateTokens(%q) = %d, want between %d and %d", tt.text, tokens, tt.min, tt.max)
			}
		})
	}
}

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

func TestIsCJK(t *testing.T) {
	tests := []struct {
		name string
		r    rune
		want bool
	}{
		{
			name: "CJK Chinese",
			r:    '你',
			want: true,
		},
		{
			name: "CJK Japanese Hiragana",
			r:    'あ',
			want: true,
		},
		{
			name: "CJK Japanese Katakana",
			r:    'ア',
			want: true,
		},
		{
			name: "CJK Korean",
			r:    '가',
			want: true,
		},
		{
			name: "ASCII letter",
			r:    'a',
			want: false,
		},
		{
			name: "ASCII number",
			r:    '1',
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCJK(tt.r); got != tt.want {
				t.Errorf("IsCJK(%q) = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}
