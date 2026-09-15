package util

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// JWTExpiry parses the exp claim from a JWT token and returns the expiry time.
// If skew is positive, the returned time is shifted earlier by that duration
// to allow for clock skew and proactive refresh.
func JWTExpiry(token string, skew time.Duration) time.Time {
	firstDot := strings.IndexByte(token, '.')
	if firstDot < 0 {
		return time.Time{}
	}
	rest := token[firstDot+1:]
	secondDot := strings.IndexByte(rest, '.')
	if secondDot < 0 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(rest[:secondDot])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	exp, err := claims.Exp.Int64()
	if err != nil || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(exp, 0).Add(-max(skew, 0))
}

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
