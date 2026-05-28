package warp

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

type LocalUserCredential struct {
	RefreshToken string `json:"refresh_token"`
	SourcePath   string `json:"source_path,omitempty"`
}

var decryptLocalUserStorageFunc = decryptLocalUserStorage
var defaultLocalUserStoragePathFunc = defaultLocalUserStoragePath

// SetLocalUserStorageTestHooks is for tests in packages that exercise API
// boundaries around WARP secure storage imports.
func SetLocalUserStorageTestHooks(pathFunc func() (string, error), decryptFunc func([]byte) (string, error)) func() {
	origPathFunc := defaultLocalUserStoragePathFunc
	origDecryptFunc := decryptLocalUserStorageFunc
	if pathFunc != nil {
		defaultLocalUserStoragePathFunc = pathFunc
	}
	if decryptFunc != nil {
		decryptLocalUserStorageFunc = decryptFunc
	}
	return func() {
		defaultLocalUserStoragePathFunc = origPathFunc
		decryptLocalUserStorageFunc = origDecryptFunc
	}
}

// ReadLocalUserCredential extracts id_token.refresh_token from WARP's local
// secure storage without exposing the full persisted User JSON.
func ReadLocalUserCredential() (*LocalUserCredential, error) {
	path, err := defaultLocalUserStoragePathFunc()
	if err != nil {
		return nil, err
	}
	return ReadLocalUserCredentialFromPath(path)
}

func ReadLocalUserCredentialFromPath(path string) (*LocalUserCredential, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("warp local user path is empty")
	}
	encrypted, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read warp local user: %w", err)
	}
	credential, err := ReadLocalUserCredentialFromBytes(encrypted)
	if err != nil {
		return nil, err
	}
	credential.SourcePath = path
	return credential, nil
}

func ReadLocalUserCredentialFromBytes(data []byte) (*LocalUserCredential, error) {
	if token := extractPlaintextLocalUserRefreshToken(string(data)); token != "" {
		return &LocalUserCredential{RefreshToken: token}, nil
	}

	plaintext, err := decryptLocalUserStorageFunc(data)
	if err != nil {
		return nil, fmt.Errorf("decrypt warp local user: %w; encrypted WARP User files can only be decrypted on the same Windows user/machine that created them, so upload decrypted User JSON or a value containing id_token.refresh_token instead", err)
	}
	token := normalizeRefreshToken(plaintext)
	if token == "" {
		return nil, fmt.Errorf("warp local user missing id_token.refresh_token")
	}
	return &LocalUserCredential{
		RefreshToken: token,
	}, nil
}

func extractPlaintextLocalUserRefreshToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if token := extractRefreshTokenFromJSON(raw); token != "" {
		return token
	}
	if token := extractRefreshTokenFromPairs(raw); token != "" {
		return token
	}
	if strings.Contains(raw, "----") {
		return extractRefreshTokenFromPairs(raw)
	}
	if isLikelyRawRefreshToken(raw) {
		return raw
	}
	return ""
}

func isLikelyRawRefreshToken(raw string) bool {
	if len(raw) < 40 || !utf8.ValidString(raw) {
		return false
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func ReadLocalUserCredentialFromReader(r io.Reader, maxBytes int64) (*LocalUserCredential, error) {
	if r == nil {
		return nil, fmt.Errorf("warp local user file is empty")
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read uploaded warp local user: %w", err)
	}
	if n > maxBytes {
		return nil, fmt.Errorf("uploaded warp local user is too large")
	}
	return ReadLocalUserCredentialFromBytes(buf.Bytes())
}
