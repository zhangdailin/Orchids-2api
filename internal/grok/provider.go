package grok

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const (
	ProviderBuild   = "build"
	ProviderWeb     = "web"
	ProviderConsole = "console"
)

const modelSnapshotTTL = 6 * time.Hour

// ProviderForAccount is the sole compatibility bridge for legacy Grok rows.
// New accounts always persist a provider; old OAuth rows are Build and old SSO
// rows remain Web until the administrator explicitly creates a Console account.
func ProviderForAccount(acc *store.Account) string {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(acc.GrokProvider)) {
	case ProviderBuild, ProviderWeb, ProviderConsole:
		return strings.ToLower(strings.TrimSpace(acc.GrokProvider))
	}
	if strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		return ProviderBuild
	}
	return ProviderWeb
}

func NormalizeProvider(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return false
	}
	provider := ProviderForAccount(acc)
	if provider == "" || acc.GrokProvider == provider {
		return false
	}
	acc.GrokProvider = provider
	return true
}

// IsLinkedConsoleSSOCompanion identifies the internal Console runtime record
// maintained for a visible Grok Web SSO source. It deliberately does not
// require a valid SSO cookie so a broken or partially migrated child cannot
// leak through account-management surfaces.
func IsLinkedConsoleSSOCompanion(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") &&
		acc.GrokSSOParentID > 0 && ProviderForAccount(acc) == ProviderConsole
}

// IsGrokWebSSOSource identifies the only administrator-manageable SSO account
// for a linked provider pair. It deliberately rejects Build OAuth, standalone
// Console, and internal Console companion records.
func IsGrokWebSSOSource(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") &&
		!strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") &&
		acc.GrokSSOParentID == 0 && ProviderForAccount(acc) == ProviderWeb
}

// CollectWebSSOSourcesByToken returns canonical visible Web SSO sources grouped
// by normalized credential. For accidental duplicate sources, the lowest ID is
// selected so callers never depend on backing-store iteration order.
func CollectWebSSOSourcesByToken(accounts []*store.Account, enabledOnly bool) map[string]*store.Account {
	result := make(map[string]*store.Account, len(accounts))
	for _, acc := range accounts {
		if !IsGrokWebSSOSource(acc) || (enabledOnly && !acc.Enabled) {
			continue
		}
		token := NormalizeSSOToken(grokSSOTokenRaw(acc))
		if token == "" {
			continue
		}
		if current := result[token]; current == nil || acc.ID < current.ID {
			result[token] = acc
		}
	}
	return result
}

// SyncWebSSOSource mirrors administrator-owned identity and scheduling fields
// into the source's linked Console runtime companion. The update starts from
// the existing child so Console-only quotas, catalogs, health, counters and
// session state remain untouched.
func SyncWebSSOSource(ctx context.Context, accountStore interface {
	ListAccounts(context.Context) ([]*store.Account, error)
	UpdateAccount(context.Context, *store.Account) error
}, source *store.Account) (bool, error) {
	if accountStore == nil || !IsGrokWebSSOSource(source) {
		return false, nil
	}
	accounts, err := accountStore.ListAccounts(ctx)
	if err != nil {
		return false, err
	}
	found := false
	for _, acc := range accounts {
		if !IsLinkedConsoleSSOCompanion(acc) || acc.GrokSSOParentID != source.ID {
			continue
		}
		found = true
		if acc.ClientCookie == source.ClientCookie && acc.UserID == source.UserID && acc.Email == source.Email && acc.TeamID == source.TeamID &&
			acc.Enabled == source.Enabled && acc.Weight == source.Weight && acc.MaxConcurrent == source.MaxConcurrent && acc.NSFWEnabled == source.NSFWEnabled {
			continue
		}
		updated := *acc
		updated.ClientCookie = source.ClientCookie
		updated.UserID = source.UserID
		updated.Email = source.Email
		updated.TeamID = source.TeamID
		updated.Enabled = source.Enabled
		updated.Weight = source.Weight
		updated.MaxConcurrent = source.MaxConcurrent
		updated.NSFWEnabled = source.NSFWEnabled
		if err := accountStore.UpdateAccount(ctx, &updated); err != nil {
			return found, fmt.Errorf("synchronize linked Grok Console SSO account: %w", err)
		}
	}
	return found, nil
}

// NewConsoleSSOCompanion creates the internal Console runtime record for a
// visible Web source without copying any provider-observed runtime state.
func NewConsoleSSOCompanion(source *store.Account) *store.Account {
	name := "grok-sso"
	if source != nil && strings.TrimSpace(source.Name) != "" {
		name = strings.TrimSpace(source.Name)
	}
	child := &store.Account{
		Name:           name + " · Console",
		AccountType:    "grok",
		CredentialType: "sso",
		GrokProvider:   ProviderConsole,
		Weight:         1,
		Enabled:        true,
		NSFWEnabled:    true,
	}
	if source == nil {
		return child
	}
	child.GrokSSOParentID = source.ID
	child.ClientCookie = source.ClientCookie
	child.UserID = source.UserID
	child.Email = source.Email
	child.TeamID = source.TeamID
	child.Weight = source.Weight
	child.MaxConcurrent = source.MaxConcurrent
	child.Enabled = source.Enabled
	child.NSFWEnabled = source.NSFWEnabled
	return child
}

// EnsureWebSSOConsoleCompanion provisions a fresh child only when the visible
// Web source has none. It never adopts an unlinked Console account solely
// because it shares a credential, preserving standalone Console runtime state.
func EnsureWebSSOConsoleCompanion(ctx context.Context, accountStore interface {
	ListAccounts(context.Context) ([]*store.Account, error)
	UpdateAccount(context.Context, *store.Account) error
	CreateAccount(context.Context, *store.Account) error
}, source *store.Account) error {
	found, err := SyncWebSSOSource(ctx, accountStore, source)
	if err != nil || found || !IsGrokWebSSOSource(source) {
		return err
	}
	if err := accountStore.CreateAccount(ctx, NewConsoleSSOCompanion(source)); err != nil {
		return fmt.Errorf("create linked Grok Console SSO account: %w", err)
	}
	return nil
}

func AccountSupportsModel(acc *store.Account, modelID string) bool {
	if acc == nil {
		return false
	}
	modelID = strings.TrimSpace(modelID)
	// Do not block a just-authorized account before its first non-billable
	// catalog sync. Once observed, the catalog is authoritative.
	if len(acc.GrokModels) == 0 {
		return true
	}
	for _, model := range acc.GrokModels {
		if strings.EqualFold(strings.TrimSpace(model), modelID) {
			return true
		}
	}
	return false
}

func CLIModelsNeedSync(acc *store.Account, now time.Time) bool {
	if ProviderForAccount(acc) != ProviderBuild || len(acc.GrokModels) == 0 || acc.GrokModelsSyncedAt.IsZero() {
		return true
	}
	return !now.Before(acc.GrokModelsSyncedAt.Add(modelSnapshotTTL))
}

func ApplyCLIModels(acc *store.Account, models []string, now time.Time) bool {
	if acc == nil {
		return false
	}
	seen := make(map[string]struct{}, len(models)+3)
	normalized := make([]string, 0, len(models)+3)
	appendModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		normalized = append(normalized, model)
	}
	for _, model := range models {
		appendModel(model)
	}

	// The snapshot records exactly what the upstream catalog returned. It used
	// to be padded with a synthetic composer entry, a 4.5 alias whenever 4.6 was
	// advertised, and a tier-gated video entry; those are locally invented
	// capabilities, and a refresh that republishes them would advertise models
	// the account never reported. Capability truth now comes from the catalog
	// alone, so a model the catalog omits is simply absent.
	if len(normalized) == 0 {
		return false
	}
	changed := !slices.EqualFunc(acc.GrokModels, normalized, strings.EqualFold)
	acc.GrokModels = normalized
	acc.GrokModelsSyncedAt = now.UTC()
	return changed
}
