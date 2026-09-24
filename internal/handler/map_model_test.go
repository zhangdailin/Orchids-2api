package handler

import "testing"

func TestMapModelOnlyNormalizesSyntax(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"claude-opus-4.6", "claude-opus-4-6"},
		{"Claude-Sonnet-4.5-Thinking", "claude-sonnet-4-5-thinking"},
		{"gpt-5.3-codex", "gpt-5.3-codex"},
		{"future-upstream-model", "future-upstream-model"},
		{"", ""},
	} {
		if got := mapModel(tc.input); got != tc.want {
			t.Errorf("mapModel(%q)=%q want %q", tc.input, got, tc.want)
		}
	}
}
