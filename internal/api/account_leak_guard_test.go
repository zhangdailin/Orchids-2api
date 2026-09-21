package api

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

// accountSecretFields is every field on a stored account that carries a
// credential, a session token or material derived from one.
//
// The list is the guard's whole point: a new credential field that nobody adds
// here is one the account API could start returning without a test noticing, so
// TestAccountSecretFieldsAreComplete fails when the struct grows one.
var accountSecretFields = map[string]func(*store.Account) string{
	"token":                  func(a *store.Account) string { return a.Token },
	"client_cookie":          func(a *store.Account) string { return a.ClientCookie },
	"refresh_token":          func(a *store.Account) string { return a.RefreshToken },
	"session_cookie":         func(a *store.Account) string { return a.SessionCookie },
	"session_id":             func(a *store.Account) string { return a.SessionID },
	"client_uat":             func(a *store.Account) string { return a.ClientUat },
	"oauth_access_token":     func(a *store.Account) string { return a.OAuthAccessToken },
	"oauth_refresh_token":    func(a *store.Account) string { return a.OAuthRefreshToken },
	"workbuddy_access_token": func(a *store.Account) string { return a.WorkBuddyAccessToken },
	"workbuddy_refresh_token": func(a *store.Account) string {
		return a.WorkBuddyRefreshToken
	},
	"qoder_access_token":  func(a *store.Account) string { return a.QoderAccessToken },
	"qoder_refresh_token": func(a *store.Account) string { return a.QoderRefreshToken },
	"qoder_runtime_info":  func(a *store.Account) string { return a.QoderRuntimeInfo },
	"qoder_runtime_key":   func(a *store.Account) string { return a.QoderRuntimeKey },
}

// accountChannels are the channels the account API serves. Every one of them goes
// through the same projection, so the guard has to cover each.
var accountChannels = []string{"warp", "grok", "puter", "workbuddy", "qoder"}

// marker prefixes every planted secret so one substring search can find all of
// them in the rendered JSON, whatever the field name became on the wire.
const marker = "PLANTED-SECRET-"

// accountWithSecrets builds an account of one channel with a distinct value in
// every credential field.
func accountWithSecrets(channel string) *store.Account {
	acc := &store.Account{ID: 42, Name: "guard", AccountType: channel, Enabled: true}
	for field, read := range accountSecretFields {
		switch read(acc) {
		case "":
			set := accountSecretSetter[field]
			set(acc, marker+field)
		}
	}
	// The status message carries upstream text verbatim, and upstream text is
	// exactly where a credential ends up when a request is echoed back. Plant
	// every value there too: both redaction layers have to strip them.
	acc.StatusMessage = "upstream rejected the request"
	for _, read := range accountSecretFields {
		if value := read(acc); value != "" {
			acc.StatusMessage += " " + value
		}
	}
	return acc
}

// accountSecretSetter mirrors accountSecretFields for the write direction.
var accountSecretSetter = map[string]func(*store.Account, string){
	"token":                  func(a *store.Account, v string) { a.Token = v },
	"client_cookie":          func(a *store.Account, v string) { a.ClientCookie = v },
	"refresh_token":          func(a *store.Account, v string) { a.RefreshToken = v },
	"session_cookie":         func(a *store.Account, v string) { a.SessionCookie = v },
	"session_id":             func(a *store.Account, v string) { a.SessionID = v },
	"client_uat":             func(a *store.Account, v string) { a.ClientUat = v },
	"oauth_access_token":     func(a *store.Account, v string) { a.OAuthAccessToken = v },
	"oauth_refresh_token":    func(a *store.Account, v string) { a.OAuthRefreshToken = v },
	"workbuddy_access_token": func(a *store.Account, v string) { a.WorkBuddyAccessToken = v },
	"workbuddy_refresh_token": func(a *store.Account, v string) {
		a.WorkBuddyRefreshToken = v
	},
	"qoder_access_token":  func(a *store.Account, v string) { a.QoderAccessToken = v },
	"qoder_refresh_token": func(a *store.Account, v string) { a.QoderRefreshToken = v },
	"qoder_runtime_info":  func(a *store.Account, v string) { a.QoderRuntimeInfo = v },
	"qoder_runtime_key":   func(a *store.Account, v string) { a.QoderRuntimeKey = v },
}

// TestAccountResponsesNeverCarryCredentials is the guard the account projection's
// refactor depends on.
//
// normalizeAccountOutputWithUsage is table-driven per channel, and its redaction
// is the last thing standing between an upstream error body and the management
// API. The test plants a distinct secret in every credential field and asserts
// none of them survives rendering, for every channel — including the fields of
// channels an account is not using, because a legacy row can hold another
// channel's value in a shared slot.
func TestAccountResponsesNeverCarryCredentials(t *testing.T) {
	for _, channel := range accountChannels {
		t.Run(channel, func(t *testing.T) {
			acc := accountWithSecrets(channel)
			raw, err := json.Marshal(normalizeAccountOutput(acc))
			if err != nil {
				t.Fatalf("marshal account output: %v", err)
			}
			if leaked := findPlantedSecrets(raw); len(leaked) > 0 {
				t.Fatalf("%s account response leaked %v\n%s", channel, leaked, raw)
			}
			// Presence, not the value, is how the table proves a credential exists.
			var row map[string]interface{}
			if err := json.Unmarshal(raw, &row); err != nil {
				t.Fatalf("decode rendered account: %v", err)
			}
			if row["has_credential"] != true {
				t.Fatalf(`${channel}: has_credential = %v, want true`, row["has_credential"])
			}
		})
	}
}

// TestAccountResponsesHideCredentialKeys proves the credential keys are absent
// rather than merely empty, so a client cannot distinguish "no credential" from
// "credential withheld" by key presence.
func TestAccountResponsesHideCredentialKeys(t *testing.T) {
	acc := accountWithSecrets("qoder")
	raw, err := json.Marshal(normalizeAccountOutput(acc))
	if err != nil {
		t.Fatalf("marshal account output: %v", err)
	}
	var row map[string]interface{}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode rendered account: %v", err)
	}
	for field := range accountSecretFields {
		if _, exists := row[field]; exists {
			t.Errorf("credential field %q was returned", field)
		}
	}
	for _, derived := range []string{"session_fingerprint", "warp_authenticated"} {
		if _, exists := row[derived]; exists {
			t.Errorf("derived field %q was returned", derived)
		}
	}
}

// findPlantedSecrets returns the planted field names still visible in raw.
func findPlantedSecrets(raw []byte) []string {
	text := string(raw)
	found := []string{}
	for field := range accountSecretFields {
		if strings.Contains(text, marker+field) {
			found = append(found, field)
		}
	}
	return found
}
