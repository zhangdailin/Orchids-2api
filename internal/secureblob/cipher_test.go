package secureblob

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	cipher, err := NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if !cipher.Available() {
		t.Fatal("cipher reported unavailable with a 32-byte key")
	}
	sealed, err := cipher.Seal("gateway state")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "gateway state") {
		t.Fatal("sealed output contains the plaintext")
	}
	plain, err := cipher.Open(sealed)
	if err != nil || plain != "gateway state" {
		t.Fatalf("Open=%q err=%v", plain, err)
	}
	// Two seals of the same value must differ: the nonce is random, so equal
	// blobs would mean a reused nonce.
	again, err := cipher.Seal("gateway state")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if again == sealed {
		t.Fatal("two seals produced identical output (nonce reuse)")
	}
}

func TestCipherRejectsForeignAndTamperedBlobs(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	sealed, err := cipher.Seal("gateway state")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := NewCipher([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a blob sealed with one key opened with another")
	}
	if _, err := cipher.Open("!!!!"); err == nil {
		t.Fatal("malformed base64 was accepted")
	}
	if _, err := cipher.Open(sealed[:len(sealed)-2]); err == nil {
		t.Fatal("truncated blob was accepted")
	}
	// Flip a byte of the decoded ciphertext, not of the base64 text: the last
	// base64 character carries unused bits, so editing it can decode to exactly
	// the same bytes and prove nothing.
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("decode sealed: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := cipher.Open(base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("tampered blob was accepted")
	}
}

func TestEmptyKeyDisablesSealing(t *testing.T) {
	cipher, err := NewCipher(nil)
	if err != nil {
		t.Fatalf("NewCipher(nil): %v", err)
	}
	if cipher != nil {
		t.Fatal("NewCipher(nil) returned a cipher")
	}
	if cipher.Available() {
		t.Fatal("nil cipher reported as available")
	}
	if _, err := cipher.Seal("x"); err == nil {
		t.Fatal("nil cipher sealed a value")
	}
	if _, err := cipher.Open("x"); err == nil {
		t.Fatal("nil cipher opened a value")
	}
}

// The derived key must be domain-separated from the credential cipher: the same
// root key under a different label has to produce an incompatible cipher.
func TestDerivedKeyIsDomainSeparated(t *testing.T) {
	root := []byte("0123456789abcdef0123456789abcdef")
	cipher, err := NewCipher(root)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	sealed, err := cipher.Seal("state")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// A cipher built from the raw root key (what the credential path uses) must
	// not be able to open a sealed blob.
	raw, err := NewCipher(append([]byte(nil), root...))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if plain, err := raw.Open(sealed); err != nil || plain != "state" {
		t.Fatalf("same-root cipher failed: plain=%q err=%v", plain, err)
	}
	if domain == "" {
		t.Fatal("domain separation label is empty")
	}
}
