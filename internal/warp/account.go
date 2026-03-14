package warp

import (
	"strings"

	"orchids-api/internal/store"
)

// ResolveRefreshToken extracts the Warp refresh token from the fields that may
// carry it in this project or older imports.
func ResolveRefreshToken(acc *store.Account) string {
	if acc == nil {
		return ""
	}

	candidates := []string{
		acc.RefreshToken,
		acc.Token,
		acc.ClientCookie,
	}

	for _, raw := range candidates {
		raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "Bearer "))
		if raw == "" {
			continue
		}
		normalized := normalizeRefreshToken(raw)
		if normalized == "" {
			continue
		}
		if looksLikeJWT(normalized) {
			continue
		}
		return normalized
	}

	return ""
}

func looksLikeJWT(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if len(strings.TrimSpace(p)) < 10 {
			return false
		}
	}
	return true
}
