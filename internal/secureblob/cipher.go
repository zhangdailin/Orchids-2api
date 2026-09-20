// Package secureblob seals gateway-owned opaque state into an authenticated,
// self-describing string.
//
// The only user so far is Grok Build remote-v2 compaction: the gateway answers a
// compaction turn itself, so the summary it produces has to travel back to the
// client inside the response and return later inside the client's history. The
// gateway keeps no server-side row for it (any account may serve the next turn),
// so the summary must be self-contained and unreadable to the holder.
package secureblob

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// domain separates this key from every other use of the credential key. Both are
// AES-GCM keys from the same root secret, and reusing one AEAD key across two
// data domains is exactly what domain separation avoids.
const domain = "orchids:secureblob:v1:"

// Cipher seals and opens blobs. A nil *Cipher is valid and reports that sealing
// is unavailable rather than panicking, so a deployment without a credential key
// degrades into "feature off".
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher derives a sealing key from the credential encryption key. An empty
// key yields (nil, nil): the caller decides whether that is fatal.
func NewCipher(rootKey []byte) (*Cipher, error) {
	if len(rootKey) == 0 {
		return nil, nil
	}
	digest := sha256.Sum256(append([]byte(domain), rootKey...))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Available reports whether sealing is configured.
func (c *Cipher) Available() bool { return c != nil && c.aead != nil }

// Seal encrypts plaintext and returns base64url text without padding.
func (c *Cipher) Seal(plaintext string) (string, error) {
	if !c.Available() {
		return "", fmt.Errorf("secure blob cipher unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open reverses Seal. A malformed or foreign blob is an error, never a silent
// empty value: callers must be able to tell "not mine" from "corrupt".
func (c *Cipher) Open(sealed string) (string, error) {
	if !c.Available() {
		return "", fmt.Errorf("secure blob cipher unavailable")
	}
	sealed = strings.TrimSpace(sealed)
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("decode secure blob: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", fmt.Errorf("secure blob is truncated")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("open secure blob: %w", err)
	}
	return string(plain), nil
}
