package grok

import (
	"encoding/base64"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/store"
)

// ApplyCLIOAuthIdentity copies only non-secret identity claims from an OAuth
// access-token JWT into the account. Claims are used as request metadata, not
// as authentication evidence; the xAI CLI endpoint remains authoritative.
func ApplyCLIOAuthIdentity(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	return ApplyCLIOAuthIdentityToken(acc, acc.OAuthAccessToken)
}

// ApplyCLIOAuthIdentityToken enriches an account from either an access_token or
// id_token JWT. Device grants commonly put email only in id_token even though
// the requested scope includes email. The token itself is never persisted by
// this helper; only the non-secret identity claims are copied.
func ApplyCLIOAuthIdentityToken(acc *store.Account, token string) bool {
	if acc == nil {
		return false
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || strings.TrimSpace(parts[1]) == "" {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Subject           string `json:"sub"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		TeamID            string `json:"team_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return false
	}
	changed := false
	if value := strings.TrimSpace(claims.Subject); value != "" && strings.TrimSpace(acc.UserID) != value {
		acc.UserID = value
		changed = true
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		candidate := strings.TrimSpace(claims.PreferredUsername)
		if strings.Contains(candidate, "@") {
			email = candidate
		}
	}
	if value := email; value != "" && strings.TrimSpace(acc.Email) != value {
		acc.Email = value
		changed = true
	}
	if email != "" && (strings.TrimSpace(acc.Name) == "" || strings.EqualFold(strings.TrimSpace(acc.Name), "grok-device-login")) {
		acc.Name = email
		changed = true
	}
	if value := strings.TrimSpace(claims.TeamID); value != "" && strings.TrimSpace(acc.TeamID) != value {
		acc.TeamID = value
		changed = true
	}
	return changed
}
