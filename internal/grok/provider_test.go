package grok

import (
	"net/http"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestIsLinkedConsoleSSOCompanion(t *testing.T) {
	tests := []struct {
		name string
		acc  *store.Account
		want bool
	}{
		{"linked console", &store.Account{AccountType: "grok", GrokProvider: ProviderConsole, GrokSSOParentID: 7}, true},
		{"linked console without cookie", &store.Account{AccountType: "grok", GrokProvider: ProviderConsole, GrokSSOParentID: 7}, true},
		{"web source", &store.Account{AccountType: "grok", GrokProvider: ProviderWeb}, false},
		{"standalone console", &store.Account{AccountType: "grok", GrokProvider: ProviderConsole}, false},
		{"build oauth", &store.Account{AccountType: "grok", CredentialType: "oauth", GrokSSOParentID: 7}, false},
		{"non grok", &store.Account{AccountType: "warp", GrokProvider: ProviderConsole, GrokSSOParentID: 7}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLinkedConsoleSSOCompanion(tt.acc); got != tt.want {
				t.Fatalf("IsLinkedConsoleSSOCompanion()=%t want %t", got, tt.want)
			}
		})
	}
}

func TestProviderForAccountSeparatesLegacyAndExplicitProviders(t *testing.T) {
	tests := []struct {
		name string
		acc  *store.Account
		want string
	}{
		{"legacy oauth", &store.Account{AccountType: "grok", CredentialType: "oauth"}, ProviderBuild},
		{"legacy sso", &store.Account{AccountType: "grok", CredentialType: "sso"}, ProviderWeb},
		{"explicit console", &store.Account{AccountType: "grok", GrokProvider: ProviderConsole}, ProviderConsole},
		{"non grok", &store.Account{AccountType: "warp"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProviderForAccount(tt.acc); got != tt.want {
				t.Fatalf("ProviderForAccount()=%q want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCapabilitySnapshotAndRateLimitsDoNotBecomeBilling(t *testing.T) {
	acc := &store.Account{
		AccountType:    "grok",
		CredentialType: "oauth",
		UsageCurrent:   8300,
		UsageLimit:     8300,
	}
	headers := make(http.Header)
	headers.Set("x-ratelimit-limit-requests", "20")
	headers.Set("x-ratelimit-remaining-requests", "19")
	headers.Set("x-ratelimit-limit-tokens", "8300")
	headers.Set("x-ratelimit-remaining-tokens", "8192")
	if !ApplyBuildRateLimits(acc, headers) {
		t.Fatal("ApplyBuildRateLimits() = false")
	}
	if acc.GrokRateLimits.Tokens.Limit != 8300 || acc.GrokBilling.Weekly.HasUsage {
		t.Fatalf("rate limits/billing mixed: %+v %+v", acc.GrokRateLimits, acc.GrokBilling)
	}
	if !ApplyCLIBillingInfo(acc, &CLIBillingInfo{UsagePercent: 12, HasUsagePercent: true, PeriodEnd: time.Now().Add(time.Hour)}) {
		t.Fatal("ApplyCLIBillingInfo() = false")
	}
	if acc.UsageCurrent != 0 || acc.UsageLimit != 0 {
		t.Fatalf("legacy flat quota not cleared: %v/%v", acc.UsageCurrent, acc.UsageLimit)
	}
	if !acc.GrokBilling.Weekly.HasUsage || acc.GrokBilling.Weekly.UsagePercent != 12 || acc.GrokRateLimits.Tokens.Limit != 8300 {
		t.Fatalf("billing/rate-limit separation lost: %+v %+v", acc.GrokBilling, acc.GrokRateLimits)
	}
}

func TestAccountSupportsModelUsesObservedBuildCatalog(t *testing.T) {
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth"}
	if !AccountSupportsModel(acc, "grok-4.6") {
		t.Fatal("unsynced account should remain eligible until its catalog is read")
	}
	ApplyCLIModels(acc, []string{"grok-4.5"}, time.Now())
	if AccountSupportsModel(acc, "grok-4.6") || !AccountSupportsModel(acc, "grok-4.5") {
		t.Fatalf("observed catalog not enforced: %#v", acc.GrokModels)
	}
}

func TestApplyCLIModelsNormalizesSparseBuildCapabilities(t *testing.T) {
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: "super"}
	ApplyCLIModels(acc, []string{"grok-4.6", "grok-imagine-video-1.5", "grok-4.6"}, time.Now())

	for _, want := range []string{"grok-4.6", "grok-4.5", "grok-composer-2.5-fast", "grok-imagine-video-1.5"} {
		if !AccountSupportsModel(acc, want) {
			t.Fatalf("normalized catalog %#v is missing %q", acc.GrokModels, want)
		}
	}
}

func TestApplyCLIModelsRemovesSuperVideoFromLowerTier(t *testing.T) {
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: "free"}
	ApplyCLIModels(acc, []string{"grok-4.6", "grok-imagine-video-1.5"}, time.Now())

	if AccountSupportsModel(acc, "grok-imagine-video-1.5") {
		t.Fatalf("free catalog retained Super-only video model: %#v", acc.GrokModels)
	}
	if !AccountSupportsModel(acc, "grok-composer-2.5-fast") {
		t.Fatalf("free Build catalog is missing composer: %#v", acc.GrokModels)
	}
}
