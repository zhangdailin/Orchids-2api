package store

import (
	"reflect"
	"strings"
	"testing"
)

// TestSecretsCoversEveryCredentialShapedField is what stops the list from drifting
// again.
//
// The redactors drifted once already: attempt diagnostics covered seven of the
// fourteen credential values, so a Qoder runtime key or a WorkBuddy refresh token
// echoed by an upstream could be persisted. A field added to the account with a
// conventional name is now a test failure rather than a silent exposure.
func TestSecretsCoversEveryCredentialShapedField(t *testing.T) {
	credentialShaped := func(name string) bool {
		lower := strings.ToLower(name)
		for _, marker := range []string{"token", "cookie", "uat", "runtimeinfo", "runtimekey", "sessionid"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
		return false
	}

	acc := &Account{}
	value := reflect.ValueOf(acc).Elem()
	typ := value.Type()

	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Kind(reflect.String) || !credentialShaped(field.Name) {
			continue
		}
		checked++
		probe := "probe-" + field.Name
		value.Field(i).SetString(probe)

		covered := false
		for _, secret := range acc.Secrets() {
			if secret == probe {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("Account.%s looks like a credential but Secrets() does not return it", field.Name)
		}
		value.Field(i).SetString("")
	}

	// A heuristic that matches nothing would pass vacuously.
	if checked == 0 {
		t.Fatal("no credential-shaped fields found; the name heuristic is broken")
	}
	t.Logf("checked %d credential-shaped fields", checked)
}

// TestSecretsSkipsEmptyValues keeps the redactors from replacing "" with a marker.
func TestSecretsReturnsOnlyNonEmptyValues(t *testing.T) {
	acc := &Account{Token: "t", ClientUat: "u"}
	var nonEmpty int
	for _, secret := range acc.Secrets() {
		if strings.TrimSpace(secret) != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 2 {
		t.Fatalf("non-empty secrets = %d, want 2", nonEmpty)
	}
}
