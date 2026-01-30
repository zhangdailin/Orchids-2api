package template

import (
	"time"
)

// formatDate formats a time.Time to string
func formatDate(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

// maskToken masks a token string for display
func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// Math functions for templates
func add(a, b int) int { return a + b }
func sub(a, b int) int { return a - b }
func mul(a, b int) int { return a * b }
func div(a, b int) int {
	if b == 0 {
		return 0
	}
	return a / b
}
func mod(a, b int) int {
	if b == 0 {
		return 0
	}
	return a % b
}

// Comparison functions
func eq(a, b interface{}) bool { return a == b }
func ne(a, b interface{}) bool { return a != b }
func lt(a, b int) bool         { return a < b }
func le(a, b int) bool         { return a <= b }
func gt(a, b int) bool         { return a > b }
func ge(a, b int) bool         { return a >= b }
