package util

import (
	"encoding/base64"
	"strings"

	"github.com/goccy/go-json"
)

// JWTEmail returns a verified-looking email claim from a JWT payload without
// validating the signature. The caller uses it only as an informational label;
// authentication remains owned by the upstream token exchange.
func JWTEmail(token string) string {
	firstDot := strings.IndexByte(token, '.')
	if firstDot < 0 {
		return ""
	}
	rest := token[firstDot+1:]
	secondDot := strings.IndexByte(rest, '.')
	if secondDot < 0 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(rest[:secondDot])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(claims.Email)
}
