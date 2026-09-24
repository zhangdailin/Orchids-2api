package grok

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

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

// TestApplyCLIModelsRecordsExactlyTheCatalog proves the capability snapshot is
// the upstream catalog and nothing else.
//
// It used to be padded with a synthetic composer entry, a 4.5 alias whenever 4.6
// was advertised, and a tier-gated video entry. Those are locally invented
// capabilities: republishing them would advertise models the account never
// reported, which is exactly what model management must not do.
func TestApplyCLIModelsRestoresGrok2APICatalogCompletion(t *testing.T) {
	// grok2api derives three entries from the account rather than the catalog: a
	// Build account advertising 4.6 can serve 4.5, an OAuth Build account can
	// serve Composer, and a Super account gets the tier-gated video entry.
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", GrokProvider: ProviderBuild, Subscription: "super"}
	ApplyCLIModels(acc, []string{"grok-4.6", "grok-imagine-video-1.5", "grok-4.6"}, time.Now())

	want := []string{"grok-4.6", "grok-imagine-video-1.5", "grok-4.5", "grok-composer-2.5-fast"}
	if len(acc.GrokModels) != len(want) {
		t.Fatalf("catalog = %#v, want %#v", acc.GrokModels, want)
	}
	for i, model := range want {
		if !strings.EqualFold(acc.GrokModels[i], model) {
			t.Fatalf("catalog = %#v, want %#v", acc.GrokModels, want)
		}
	}
	for _, derived := range want[2:] {
		if !AccountSupportsModel(acc, derived) {
			t.Fatalf("derived capability %q is missing: %#v", derived, acc.GrokModels)
		}
	}
	if acc.GrokModelsSyncedAt.IsZero() {
		t.Fatal("the snapshot was not dated")
	}
}

// The only capability grok2api gates on tier is the video 1.5 entry: a Super
// account gains it, anything below loses it even if the catalog listed it.
