package handler

import (
	"testing"
)

func TestClassifyAccountStatus(t *testing.T) {
	tests := []struct {
		name     string
		errStr   string
		expected string
	}{
		{
			name:     "Explicit 401",
			errStr:   "HTTP 401 Unauthorized",
			expected: "401",
		},
		{
			name:     "Explicit 403",
			errStr:   "HTTP 403 Forbidden",
			expected: "403",
		},
		{
			name:     "Explicit 404",
			errStr:   "HTTP 404 Not Found",
			expected: "404",
		},
		{
			name:     "Signed out message",
			errStr:   "User is signed out",
			expected: "401",
		},
		{
			name:     "Forbidden message",
			errStr:   "Access forbidden",
			expected: "403",
		},
		{
			name:     "Explicit 429",
			errStr:   "HTTP 429 Too Many Requests",
			expected: "429",
		},
		{
			name:     "Quota exceeded message",
			errStr:   "No remaining quota: No AI requests remaining",
			expected: "429",
		},
		{
			name:     "Rate limit message",
			errStr:   "Rate limit exceeded",
			expected: "429",
		},
		{
			name:     "Credits exhausted message",
			errStr:   "You have run out of credits. Please upgrade your plan to continue.",
			expected: "429",
		},
		{
			name:     "Server error (ignored)",
			errStr:   "HTTP 500 Internal Server Error",
			expected: "",
		},
		{
			name:     "Unknown error (ignored)",
			errStr:   "Something went wrong",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAccountStatus(tt.errStr)
			if got != tt.expected {
				t.Errorf("classifyAccountStatus(%q) = %q, want %q", tt.errStr, got, tt.expected)
			}
		})
	}
}
