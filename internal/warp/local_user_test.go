package warp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLocalUserCredentialFromPath_ExtractsNestedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.warp.Warp-User")
	if err := os.WriteFile(path, []byte("encrypted"), 0o600); err != nil {
		t.Fatalf("write temp user: %v", err)
	}

	orig := decryptLocalUserStorageFunc
	decryptLocalUserStorageFunc = func(encrypted []byte) (string, error) {
		if string(encrypted) != "encrypted" {
			t.Fatalf("encrypted=%q want encrypted", encrypted)
		}
		return `{"id_token":{"id_token":"runtime-jwt","refresh_token":"token-123"},"refresh_token":""}`, nil
	}
	t.Cleanup(func() {
		decryptLocalUserStorageFunc = orig
	})

	credential, err := ReadLocalUserCredentialFromPath(path)
	if err != nil {
		t.Fatalf("ReadLocalUserCredentialFromPath() error: %v", err)
	}
	if credential.RefreshToken != "token-123" {
		t.Fatalf("RefreshToken=%q want token-123", credential.RefreshToken)
	}
	if credential.SourcePath != path {
		t.Fatalf("SourcePath=%q want %q", credential.SourcePath, path)
	}
}

func TestReadLocalUserCredentialFromBytes_ExtractsPlaintextNestedToken(t *testing.T) {
	credential, err := ReadLocalUserCredentialFromBytes([]byte(`{"id_token":{"id_token":"runtime-jwt","refresh_token":"token-from-json"},"refresh_token":""}`))
	if err != nil {
		t.Fatalf("ReadLocalUserCredentialFromBytes() error: %v", err)
	}
	if credential.RefreshToken != "token-from-json" {
		t.Fatalf("RefreshToken=%q want token-from-json", credential.RefreshToken)
	}
}

func TestReadLocalUserCredentialFromBytes_ExtractsLikelyRawToken(t *testing.T) {
	credential, err := ReadLocalUserCredentialFromBytes([]byte("raw_refresh_token_value_abcdefghijklmnopqrstuvwxyz1234567890"))
	if err != nil {
		t.Fatalf("ReadLocalUserCredentialFromBytes() error: %v", err)
	}
	if credential.RefreshToken != "raw_refresh_token_value_abcdefghijklmnopqrstuvwxyz1234567890" {
		t.Fatalf("RefreshToken=%q", credential.RefreshToken)
	}
}

func TestReadLocalUserCredentialFromBytes_ExplainsEncryptedUploadLimit(t *testing.T) {
	orig := decryptLocalUserStorageFunc
	decryptLocalUserStorageFunc = func([]byte) (string, error) {
		return "", os.ErrPermission
	}
	t.Cleanup(func() {
		decryptLocalUserStorageFunc = orig
	})

	_, err := ReadLocalUserCredentialFromBytes([]byte("encrypted"))
	if err == nil {
		t.Fatal("ReadLocalUserCredentialFromBytes() error is nil")
	}
	if !strings.Contains(err.Error(), "upload decrypted User JSON") {
		t.Fatalf("error=%q want decrypted User JSON hint", err.Error())
	}
}

func TestReadLocalUserCredential_RealStorage(t *testing.T) {
	if os.Getenv("WARP_LOCAL_USER_REAL") != "1" {
		t.Skip("set WARP_LOCAL_USER_REAL=1 to verify the current Windows user's WARP secure storage")
	}

	credential, err := ReadLocalUserCredential()
	if err != nil {
		t.Fatalf("ReadLocalUserCredential() error: %v", err)
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		t.Fatal("RefreshToken is empty")
	}
	if strings.TrimSpace(credential.SourcePath) == "" {
		t.Fatal("SourcePath is empty")
	}
}
