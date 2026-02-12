package api

import "strings"

// isLikelyJWT returns true when the input looks like a JWT (three dot-separated parts).
// We keep this intentionally loose to accept most JWTs pasted by users.
func isLikelyJWT(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(p) < 10 {
			return false
		}
	}
	return true
}
