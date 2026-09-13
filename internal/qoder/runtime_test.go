package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
)

// recordingSource replays a fixed byte sequence and fails when it runs dry, so
// a derivation that reads more entropy than the fixture provides is a test
// failure rather than a silent zero-filled key. Reads of any size are served
// from the same stream, because the RSA padding is drawn one byte at a time
// while the UUID and the runtime key are drawn in blocks.
type recordingSource struct {
	data []byte
	pos  int
}

func newRecordingSource(chunks ...[]byte) *recordingSource {
	var joined []byte
	for _, chunk := range chunks {
		joined = append(joined, chunk...)
	}
	return &recordingSource{data: joined}
}

func (r *recordingSource) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errEntropyExhausted
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

var errEntropyExhausted = &entropyExhaustedError{}

type entropyExhaustedError struct{}

func (*entropyExhaustedError) Error() string { return "test entropy source exhausted" }

// TestRuntimeFieldsMatchesPinnedFixture pins the derived pair against the
// synthetic fixture that fixes the Qoder CLI v1.1.34 byte layout.
//
// This is the single most load-bearing test in the package: the upstream does
// not re-derive or verify these fields, it simply fails every request whose
// encryption or key-wrapping differs. A silent change here would break the
// channel in a way that looks like an expired credential.
func TestRuntimeFieldsMatchesPinnedFixture(t *testing.T) {
	t.Parallel()

	uuidEntropy, err := base64.StdEncoding.DecodeString("AQIDBAUGBwgJCgsMDQ4PEA==")
	if err != nil {
		t.Fatalf("decode uuid entropy: %v", err)
	}
	paddingEntropy, err := base64.StdEncoding.DecodeString("ERITFBUWFxgZGhscHR4fICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj9AQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVpbXF1eX2BhYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ent8fQ==")
	if err != nil {
		t.Fatalf("decode padding entropy: %v", err)
	}

	source := newRecordingSource(uuidEntropy, paddingEntropy)
	fields, err := runtimeFieldsFor(source, runtimeFieldInput{
		UID:              "synthetic-user-0001",
		OrganizationID:   "synthetic-org-0001",
		OrganizationTags: []string{"synthetic-a", "b"},
		DataPolicyAgreed: true,
	})
	if err != nil {
		t.Fatalf("runtimeFieldsFor() error = %v", err)
	}

	const wantInfo = "A2MBsulMvdx5p3X6li/3gjwEdVeJ/EkXILah2VTr11+i+FqvTaZ90ZrsGAtfSUode28WR718Kzups7BnrO2w+LGj2Y3+3zswmr48XkCV+HvUuajHkcU+tPJdWeL69JwKA+h2be5MvPMEYFopCPt9jE/DBdk/2Q+1oBab6DYLbrp8u5IYpZq7Fe4IrCcskN0K"
	const wantKey = "nKeC6jJtXTWF4TpnVNnxMT/6jY0gBZfCPfjQaKbIi0lS+j2REWVXO5f6BzlvfEvHCrC+UY7rBMZfYs/C72KNYVIH2HLAR8E1yPDAGvV2xViMw029bXcMVa9ib0vyaM4IEu7HJlNHzUXTJOvEBB4YUAyxTCysQ/oYct/JIpRNg7I="

	if fields.EncryptUserInfo != wantInfo {
		t.Errorf("encrypt_user_info = %q, want %q", fields.EncryptUserInfo, wantInfo)
	}
	if fields.Key != wantKey {
		t.Errorf("key = %q, want %q", fields.Key, wantKey)
	}
}

// TestReverseMaskUUID pins the key derivation. The AES key is the hex of the
// reversed, masked UUID bytes kept as ASCII; hex-decoding it instead would
// produce an 8-byte key and a different ciphertext.
func TestReverseMaskUUID(t *testing.T) {
	t.Parallel()

	raw := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	masked := reverseMaskUUID(raw)
	if got, want := formatUUID(masked), "100f0e0d-0c0b-4a09-8807-060504030201"; got != want {
		t.Fatalf("formatUUID() = %q, want %q", got, want)
	}
	if got, want := string(runtimeASCIIKey(masked)), "100f0e0d0c0b4a09"; got != want {
		t.Fatalf("runtimeASCIIKey() = %q, want %q", got, want)
	}
	if len(runtimeASCIIKey(masked)) != 16 {
		t.Fatalf("runtimeASCIIKey() length = %d, want 16", len(runtimeASCIIKey(masked)))
	}
}

// TestRuntimeFieldsAccessorRoundTrip proves a stored pair is accepted and a half
// pair is rejected, so a partially written account re-derives instead of sending
// an unusable header.
func TestRuntimeFieldsAccessorRoundTrip(t *testing.T) {
	t.Parallel()

	if (RuntimeFields{}).Complete() {
		t.Fatal("Complete() = true for an empty pair")
	}
	if (RuntimeFields{Key: "x"}).Complete() {
		t.Fatal("Complete() = true for a pair with no info")
	}
	if !(RuntimeFields{EncryptUserInfo: "a", Key: "b"}).Complete() {
		t.Fatal("Complete() = false for a full pair")
	}
}

// TestRuntimeFieldInputAlwaysCarriesTagsArray pins the empty-array encoding: the
// gateway's own serializer emits `[]`, and omitting the key would change the
// ciphertext.
func TestRuntimeFieldInputAlwaysCarriesTagsArray(t *testing.T) {
	t.Parallel()

	source := newRecordingSource(bytes.Repeat([]byte{7}, 16), bytes.Repeat([]byte{9}, 109))
	fields, err := runtimeFieldsFor(source, runtimeFieldInput{UID: "u"})
	if err != nil {
		t.Fatalf("runtimeFieldsFor() error = %v", err)
	}
	// Decrypt with the derived key to prove the plaintext contains "[]".
	key := runtimeASCIIKey(reverseMaskUUID([16]byte{7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7}))
	sealed, err := base64.StdEncoding.DecodeString(fields.EncryptUserInfo)
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("build cipher: %v", err)
	}
	plaintext := make([]byte, len(sealed))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plaintext, sealed)
	unpadded := unpadPKCS7(t, plaintext)
	if !strings.Contains(string(unpadded), `"organization_tags":[]`) {
		t.Fatalf("plaintext = %s, want an empty organization_tags array", unpadded)
	}
}

func unpadPKCS7(t *testing.T, padded []byte) []byte {
	t.Helper()
	if len(padded) == 0 {
		t.Fatal("padded plaintext is empty")
	}
	padding := int(padded[len(padded)-1])
	if padding <= 0 || padding > len(padded) {
		t.Fatalf("invalid padding %d", padding)
	}
	return padded[:len(padded)-padding]
}

// TestPKCEPairShape pins the verifier/challenge contract: a 43..128 character
// unreserved verifier and an unpadded base64url SHA-256 challenge.
func TestPKCEPairShape(t *testing.T) {
	t.Parallel()

	source := newRecordingSource([]byte{0}, bytes.Repeat([]byte{0x42}, 43))
	verifier, challenge, err := pkcePair(source)
	if err != nil {
		t.Fatalf("pkcePair() error = %v", err)
	}
	if len(verifier) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(verifier))
	}
	if strings.ContainsAny(challenge, "+/=") {
		t.Fatalf("challenge %q is not unpadded base64url", challenge)
	}
	if len(challenge) != 43 && len(challenge) != 42 {
		// A 32-byte digest is 43 base64url characters once padding is dropped.
		t.Fatalf("challenge length = %d, want 43", len(challenge))
	}
	// A verifier outside the unreserved set would be rejected by the browser
	// page, so the charset is asserted rather than assumed.
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	for _, r := range verifier {
		if !strings.ContainsRune(allowed, r) {
			t.Fatalf("verifier contains %q, which is outside the allowed charset", r)
		}
	}
}

// TestPKCEVerifierLengthSpread covers the upper bound of the verifier length.
func TestPKCEVerifierLengthSpread(t *testing.T) {
	t.Parallel()

	source := newRecordingSource([]byte{85}, bytes.Repeat([]byte{0x11}, 128))
	verifier, _, err := pkcePair(source)
	if err != nil {
		t.Fatalf("pkcePair() error = %v", err)
	}
	if len(verifier) != 128 {
		t.Fatalf("verifier length = %d, want 128", len(verifier))
	}
}

// TestNewUUIDVersionAndVariant pins the UUID shape the upstream expects in the
// nonce and machine id parameters.
func TestNewUUIDVersionAndVariant(t *testing.T) {
	t.Parallel()

	source := newRecordingSource(bytes.Repeat([]byte{0xff}, 16))
	value, err := newUUID(source)
	if err != nil {
		t.Fatalf("newUUID() error = %v", err)
	}
	if len(value) != 36 {
		t.Fatalf("uuid length = %d, want 36", len(value))
	}
	if value[14] != '4' {
		t.Fatalf("uuid = %q, want version 4 at index 14", value)
	}
	if !strings.ContainsRune("89ab", rune(value[19])) {
		t.Fatalf("uuid = %q, want an RFC 4122 variant at index 19", value)
	}
}

// TestRSAEncryptionRejectsShortModulusInput guards the padding arithmetic.
func TestRSAEncryptionRejectsShortModulusInput(t *testing.T) {
	t.Parallel()

	publicKey, err := runtimePublicKey()
	if err != nil {
		t.Fatalf("runtimePublicKey() error = %v", err)
	}
	source := newRecordingSource()
	if _, err := rsaEncryptPKCS1v15WithSource(source, publicKey, make([]byte, publicKey.Size())); err == nil {
		t.Fatal("rsaEncryptPKCS1v15WithSource() error = nil for an oversized message")
	}
}
