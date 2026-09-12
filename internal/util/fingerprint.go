package util

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Fingerprint returns a short, stable, non-reversible identifier for a secret.
//
// It exists so an operator can tell two credentials apart in the management UI
// without the credential itself ever leaving the server: some channels (Warp in
// particular) authenticate with a session token that carries no email or
// username, so the account table had nothing to show but "登录会话已配置".
//
// Twelve hex characters is enough to distinguish live sessions while being
// useless for brute-forcing the original value.
func Fingerprint(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(digest[:6])
}
