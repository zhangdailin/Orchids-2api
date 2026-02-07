package handler

import "testing"

func TestMapModel(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Opus 4.6 系列
		{"claude-opus-4-6", "claude-opus-4-6"},
		{"claude-opus-4.6", "claude-opus-4-6"},
		{"claude-4-6-opus-high", "claude-opus-4-6"},
		{"claude-4-6-opus-max", "claude-opus-4-6"},
		{"claude-opus-4-6-thinking", "claude-opus-4-6-thinking"},
		{"claude-opus-4.6-thinking", "claude-opus-4-6-thinking"},

		// Opus 4.5 系列
		{"claude-opus-4-5", "claude-opus-4-5"},
		{"claude-opus-4.5", "claude-opus-4-5"},
		{"claude-4-5-opus", "claude-opus-4-5"},
		{"claude-opus-4-5-thinking", "claude-opus-4-5-thinking"},
		{"claude-opus-4.5-thinking", "claude-opus-4-5-thinking"},
		{"claude-4-5-opus-thinking", "claude-opus-4-5-thinking"},

		// Opus 通配
		{"claude-opus", "claude-opus-4-6"},
		{"opus", "claude-opus-4-6"},
		{"opus-thinking", "claude-opus-4-6-thinking"},

		// Sonnet 3.7 系列
		{"claude-sonnet-3-7", "claude-3-7-sonnet-20250219"},
		{"claude-sonnet-3.7", "claude-3-7-sonnet-20250219"},
		{"claude-3-7-sonnet", "claude-3-7-sonnet-20250219"},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-20250219"},

		// Sonnet 3.5 系列 (升级到 4.5)
		{"claude-sonnet-3-5", "claude-sonnet-4-5"},
		{"claude-sonnet-3.5", "claude-sonnet-4-5"},
		{"claude-3-5-sonnet", "claude-sonnet-4-5"},
		{"claude-3-5-sonnet-20241022", "claude-sonnet-4-5"},

		// Sonnet 4.5 系列
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"claude-sonnet-4.5", "claude-sonnet-4-5"},
		{"claude-4-5-sonnet", "claude-sonnet-4-5"},
		{"claude-sonnet-4-5-thinking", "claude-sonnet-4-5-thinking"},
		{"claude-sonnet-4.5-thinking", "claude-sonnet-4-5-thinking"},
		{"claude-4-5-sonnet-thinking", "claude-sonnet-4-5-thinking"},

		// Sonnet 4 精确版本号
		{"claude-sonnet-4-20250514", "claude-sonnet-4-20250514"},

		// Sonnet 4 / Sonnet 通配
		{"claude-sonnet-4", "claude-sonnet-4-20250514"},
		{"sonnet", "claude-sonnet-4-20250514"},
		{"claude-sonnet", "claude-sonnet-4-20250514"},
		{"claude-sonnet-4-thinking", "claude-sonnet-4-5-thinking"},

		// Haiku 4.5 系列
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"claude-haiku-4.5", "claude-haiku-4-5"},
		{"claude-4-5-haiku", "claude-haiku-4-5"},

		// Haiku 通配
		{"claude-haiku", "claude-haiku-4-5"},
		{"haiku", "claude-haiku-4-5"},

		// 默认
		{"", "claude-sonnet-4-5"},
		{"unknown-model", "claude-sonnet-4-5"},
		{"gpt-4o", "claude-sonnet-4-5"},

		// 大小写混合
		{"Claude-Opus-4-5", "claude-opus-4-5"},
		{"CLAUDE-SONNET-4-5-THINKING", "claude-sonnet-4-5-thinking"},
		{"Claude-Haiku-4.5", "claude-haiku-4-5"},
		{"OPUS-4-6-MAX", "claude-opus-4-6"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapModel(tt.input)
			if got != tt.want {
				t.Errorf("mapModel(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
