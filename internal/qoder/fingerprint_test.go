package qoder

import (
	"context"
	"strings"
	"testing"
)

// 参考值由 qoder2api 的 internal/cosy/fingerprint.go 对同一 seed 计算所得
//（交叉校验），仅在「无本机盐」的种子下成立，测试入口统一 SetInstallSalt("")。
//
//	machine 005b8945c0659064f8d25299980a27b3
//	mtype   66ea01f7983702b088
//	mtoken  zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0
const testFingerprintUID = "test-uid-123"

func TestDeriveMatchesReference(t *testing.T) {
	t.Cleanup(func() { SetInstallSalt("") })
	SetInstallSalt("")
	if got := DeriveMachineID(testFingerprintUID); got != "005b8945c0659064f8d25299980a27b3" {
		t.Errorf("DeriveMachineID = %s", got)
	}
	if got := DeriveMachineType(testFingerprintUID); got != "66ea01f7983702b088" {
		t.Errorf("DeriveMachineType = %s", got)
	}
	if got := DeriveMachineToken(testFingerprintUID); got != "zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0" {
		t.Errorf("DeriveMachineToken = %s", got)
	}
}

func TestDeriveStableAndIsolated(t *testing.T) {
	t.Cleanup(func() { SetInstallSalt("") })
	SetInstallSalt("")
	if DeriveMachineID("acct-a") != DeriveMachineID("acct-a") {
		t.Fatal("machine id not stable for the same uid")
	}
	if DeriveMachineID("acct-a") == DeriveMachineID("acct-b") {
		t.Fatal("machine id collides across accounts")
	}
	if len(DeriveMachineID(testFingerprintUID)) != 32 {
		t.Errorf("machineid len = %d, want 32", len(DeriveMachineID(testFingerprintUID)))
	}
	if len(DeriveMachineType(testFingerprintUID)) != 18 {
		t.Errorf("machinetype len = %d, want 18", len(DeriveMachineType(testFingerprintUID)))
	}
	if len(DeriveMachineToken(testFingerprintUID)) != 43 {
		t.Errorf("machinetoken len = %d, want 43", len(DeriveMachineToken(testFingerprintUID)))
	}
	// The UID wins over the credential, and the fallback is stable too.
	seed := FingerprintSeed("", "pt-secret")
	if FingerprintSeed("", "pt-secret") != seed {
		t.Fatal("credential seed is not stable")
	}
	if FingerprintSeed("real-uid", "pt-secret") == seed {
		t.Fatal("uid seed must take precedence over the credential seed")
	}
	if seed != "cred:pt-secret" {
		t.Fatalf("credential seed = %q", seed)
	}
}

func TestDeriveInstallSalt(t *testing.T) {
	t.Cleanup(func() { SetInstallSalt("") })
	SetInstallSalt("")
	plainID := DeriveMachineID(testFingerprintUID)
	plainToken := DeriveMachineToken(testFingerprintUID)

	SetInstallSalt("unit-test-salt")
	saltedID := DeriveMachineID(testFingerprintUID)
	saltedToken := DeriveMachineToken(testFingerprintUID)
	if saltedID == plainID || len(saltedID) != 32 {
		t.Errorf("salted machineid invalid or equal to the plain one: %s", saltedID)
	}
	if saltedToken == plainToken || len(saltedToken) != 43 {
		t.Errorf("salted machinetoken invalid or equal to the plain one: %s", saltedToken)
	}
	if DeriveMachineID(testFingerprintUID) != saltedID {
		t.Error("salted derivation is not idempotent")
	}
	if DeriveMachineID("acct-a") == DeriveMachineID("acct-b") {
		t.Fatal("salted machine id collides across accounts")
	}
	SetInstallSalt("another-salt")
	if DeriveMachineID(testFingerprintUID) == saltedID {
		t.Error("changing the salt must change the fingerprint")
	}
	SetInstallSalt("")
	if DeriveMachineID(testFingerprintUID) != plainID || DeriveMachineToken(testFingerprintUID) != plainToken {
		t.Error("clearing the salt must restore the reference fingerprint")
	}
}

func TestFingerprintForKeepsTheBoundMachineID(t *testing.T) {
	t.Cleanup(func() { SetInstallSalt("") })
	SetInstallSalt("")

	first := FingerprintFor("11111111-2222-4333-8444-555555555555", "uid-1", "access-1")
	if first.MachineID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("MachineID = %q, want the recorded device id", first.MachineID)
	}
	if first.Token == first.MachineID {
		t.Fatal("the device token must differ from the device id")
	}
	if first.Type == "" || first.Type == first.MachineID {
		t.Fatalf("MachineType = %q, want a derived value", first.Type)
	}

	// Stable across calls and across a restart: the same account signs every
	// request from the same virtual device.
	again := FingerprintFor("11111111-2222-4333-8444-555555555555", "uid-1", "access-1")
	if again != first {
		t.Fatalf("fingerprint drifted: %+v then %+v", first, again)
	}
	// A different account is a different device.
	other := FingerprintFor("99999999-2222-4333-8444-555555555555", "uid-2", "access-2")
	if other.Token == first.Token || other.Type == first.Type {
		t.Fatal("fingerprints are not isolated per account")
	}
	// Without any seed there is nothing to derive, and nothing is invented.
	if got := (FingerprintFor("", "", "")); got.Token != "" || got.Type != "" {
		t.Fatalf("empty seed derived a fingerprint: %+v", got)
	}
	if got := FingerprintFor("machine-only", "", ""); got.Token == "" || got.Type == "" {
		t.Fatal("a recorded device id must still seed the derivation")
	}
	if !strings.HasPrefix(FingerprintSeed("", "machine-only"), "cred:") {
		t.Fatal("the device id fallback should travel as a credential seed")
	}
}

type stubSettings struct {
	values map[string]string
	writes int
}

func (s *stubSettings) GetSetting(_ context.Context, key string) (string, error) {
	return s.values[key], nil
}

func (s *stubSettings) SetSetting(_ context.Context, key, value string) error {
	s.writes++
	if s.values == nil {
		s.values = map[string]string{}
	}
	s.values[key] = value
	return nil
}

// TestEnsureInstallSaltPersistsOnce proves the salt is generated exactly once
// and then read back: rotating it would move every account's fingerprint at
// once, which is the device change the stabilization exists to avoid.
func TestEnsureInstallSaltPersistsOnce(t *testing.T) {
	t.Cleanup(func() { SetInstallSalt("") })
	SetInstallSalt("")

	settings := &stubSettings{}
	first := EnsureInstallSalt(context.Background(), settings)
	if len(first) != 64 {
		t.Fatalf("salt = %q, want 32 hex-encoded bytes", first)
	}
	if InstallSalt() != first {
		t.Fatal("the generated salt was not put into effect")
	}
	if settings.writes != 1 {
		t.Fatalf("writes = %d, want one", settings.writes)
	}

	// A second start reads the stored value instead of minting a new one.
	SetInstallSalt("")
	second := EnsureInstallSalt(context.Background(), settings)
	if second != first {
		t.Fatalf("salt rotated on the second start: %q then %q", first, second)
	}
	if settings.writes != 1 {
		t.Fatalf("writes = %d, want the stored salt not to be rewritten", settings.writes)
	}

	// Without a store there is no durable salt; the process keeps the
	// reference-compatible derivation rather than inventing a throwaway one.
	SetInstallSalt("")
	if got := EnsureInstallSalt(context.Background(), nil); got != "" {
		t.Fatalf("salt without a store = %q, want empty", got)
	}
}

// TestAliyunUserTypeFallsBackToADocumentedClass pins the account class in the
// chat body: the upstream sorts a request into a queue by it, so an unknown
// class must still send a class it recognises rather than an empty field.
func TestAliyunUserTypeFallsBackToADocumentedClass(t *testing.T) {
	if got := aliyunUserTypeOr(""); got != defaultAliyunUserType || got == "" {
		t.Fatalf("aliyunUserTypeOr(\"\") = %q, want a documented default", got)
	}
	if got := aliyunUserTypeOr("personal_professional_trial"); got != "personal_professional_trial" {
		t.Fatalf("aliyunUserTypeOr(known) = %q, want the account's own class", got)
	}
	if got := aliyunUserTypeOr("  "); got != defaultAliyunUserType {
		t.Fatalf("aliyunUserTypeOr(blank) = %q, want the default", got)
	}

	client := NewFromAccount(nil, nil)
	if got := client.aliyunUserType(); got != "" {
		t.Fatalf("an account-less client reports class %q, want empty so the default applies", got)
	}
}
