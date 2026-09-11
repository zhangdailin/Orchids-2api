package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

// EnsureGrokSSOProviderViews keeps an internal Console runtime account beside
// every visible Web SSO source. The accounts share credential identity and
// source-owned scheduling only; provider-specific quotas, model snapshots,
// health, request counters and session state remain independent.
func (a *API) EnsureGrokSSOProviderViews(ctx context.Context) error {
	if a == nil || a.store == nil {
		return nil
	}
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	groups := make(map[string][]*store.Account)
	for _, acc := range accounts {
		if !isGrokSSOAccount(acc) {
			continue
		}
		token := grok.NormalizeSSOToken(acc.ClientCookie)
		if token != "" {
			groups[token] = append(groups[token], acc)
		}
	}
	for _, group := range groups {
		// A broken child can be missing its cookie and therefore cannot identify a
		// credential group on its own. Attach it through its durable parent link
		// so reconciliation repairs it instead of creating a second companion.
		seen := make(map[int64]struct{}, len(group))
		sourceIDs := make(map[int64]struct{}, len(group))
		for _, acc := range group {
			if acc == nil {
				continue
			}
			seen[acc.ID] = struct{}{}
			if grok.ProviderForAccount(acc) == grok.ProviderWeb && acc.GrokSSOParentID == 0 {
				sourceIDs[acc.ID] = struct{}{}
			}
		}
		for _, acc := range accounts {
			if !grok.IsLinkedConsoleSSOCompanion(acc) {
				continue
			}
			if _, belongsToSource := sourceIDs[acc.GrokSSOParentID]; !belongsToSource {
				continue
			}
			if _, alreadyIncluded := seen[acc.ID]; alreadyIncluded {
				continue
			}
			group = append(group, acc)
			seen[acc.ID] = struct{}{}
		}
		if err := a.ensureGrokSSOProviderGroup(ctx, group); err != nil {
			return err
		}
	}
	return nil
}

func isGrokSSOAccount(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") &&
		!grokAccountIsOAuth(acc) && grok.NormalizeSSOToken(acc.ClientCookie) != ""
}

// isLinkedGrokConsoleAccount identifies an internal Console runtime child.
func isLinkedGrokConsoleAccount(acc *store.Account) bool {
	return grok.IsLinkedConsoleSSOCompanion(acc)
}

func (a *API) ensureGrokSSOProviderGroup(ctx context.Context, group []*store.Account) error {
	if len(group) == 0 {
		return nil
	}
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })

	// A credential group has exactly one manageable Web source. Do not try to
	// repair ambiguous source ownership by deleting or reparenting records: that
	// could make an unrelated Console runtime account disappear from routing.
	var sources []*store.Account
	for _, acc := range group {
		if grok.ProviderForAccount(acc) != grok.ProviderWeb {
			continue
		}
		if acc.GrokSSOParentID != 0 {
			return fmt.Errorf("Grok Web SSO account %d has unexpected parent %d", acc.ID, acc.GrokSSOParentID)
		}
		sources = append(sources, acc)
	}
	if len(sources) > 1 {
		return fmt.Errorf("Grok SSO credential has conflicting visible Web sources %d and %d", sources[0].ID, sources[1].ID)
	}

	var source *store.Account
	if len(sources) == 1 {
		source = sources[0]
	}

	// Validate all existing links before changing the group. An already-linked
	// Console account belongs only to its current live source; it is never an
	// adoption candidate for a different source.
	for _, acc := range group {
		if grok.ProviderForAccount(acc) != grok.ProviderConsole || acc.GrokSSOParentID == 0 {
			continue
		}
		if source == nil || acc.GrokSSOParentID != source.ID {
			return fmt.Errorf("Grok Console SSO account %d is linked to different source %d", acc.ID, acc.GrokSSOParentID)
		}
	}

	if source == nil {
		// Preserve a legacy Console record's provider runtime state and add a
		// linked Web source instead of reclassifying the Console record.
		source = newGrokSSOProviderView(group[0], grok.ProviderWeb, 0)
		if err := a.store.CreateAccount(ctx, source); err != nil {
			return fmt.Errorf("create Grok Web SSO source: %w", err)
		}
	}

	var linkedConsoles []*store.Account
	var unlinkedConsoles []*store.Account
	for _, acc := range group {
		if grok.ProviderForAccount(acc) != grok.ProviderConsole {
			continue
		}
		switch acc.GrokSSOParentID {
		case source.ID:
			linkedConsoles = append(linkedConsoles, acc)
		case 0:
			unlinkedConsoles = append(unlinkedConsoles, acc)
		default:
			// The earlier validation makes this unreachable for existing sources,
			// but retain the guard in case a freshly created source receives a
			// malformed group from a future caller.
			return fmt.Errorf("Grok Console SSO account %d is linked to different source %d", acc.ID, acc.GrokSSOParentID)
		}
	}

	var console *store.Account
	if len(linkedConsoles) > 0 {
		// group is ID-sorted, so keep the lowest-ID child already owned by the
		// canonical source before considering an unlinked legacy Console row.
		console = linkedConsoles[0]
	} else if len(unlinkedConsoles) > 0 {
		console = unlinkedConsoles[0]
		linked := *console
		linked.GrokSSOParentID = source.ID
		if err := a.store.UpdateAccount(ctx, &linked); err != nil {
			return fmt.Errorf("link Grok Console SSO account: %w", err)
		}
		console = &linked
	} else {
		console = newGrokSSOProviderView(source, grok.ProviderConsole, source.ID)
		if err := a.store.CreateAccount(ctx, console); err != nil {
			return fmt.Errorf("create linked Grok Console SSO account: %w", err)
		}
	}

	for _, acc := range append(linkedConsoles, unlinkedConsoles...) {
		if acc.ID == console.ID {
			continue
		}
		if err := a.store.DeleteAccount(ctx, acc.ID); err != nil {
			return fmt.Errorf("delete redundant Grok Console SSO account: %w", err)
		}
	}
	return a.syncGrokSSOProviderView(ctx, source)
}

func newGrokSSOProviderView(source *store.Account, provider string, parentID int64) *store.Account {
	name := "grok-sso"
	if source != nil && strings.TrimSpace(source.Name) != "" {
		name = strings.TrimSpace(source.Name)
	}
	label := "Web"
	if provider == grok.ProviderConsole {
		label = "Console"
	}
	view := &store.Account{
		Name:            name + " · " + label,
		AccountType:     "grok",
		CredentialType:  "sso",
		GrokProvider:    provider,
		GrokSSOParentID: parentID,
		Weight:          1,
		Enabled:         true,
		NSFWEnabled:     true,
	}
	if source == nil {
		return view
	}
	view.ClientCookie = source.ClientCookie
	view.UserID = source.UserID
	view.Email = source.Email
	view.TeamID = source.TeamID
	// The visible source owns configuration that operators can manage. Console
	// provider runtime state remains exclusive to the internal child.
	view.Weight = source.Weight
	view.MaxConcurrent = source.MaxConcurrent
	view.Enabled = source.Enabled
	view.NSFWEnabled = source.NSFWEnabled
	return view
}

// reconcileGrokSSOProviderCredential repairs one legacy or partially provisioned
// SSO credential group before duplicate detection. This makes a retry restore a
// missing companion instead of leaving a durable source record that can only
// produce a conflict.
func (a *API) reconcileGrokSSOProviderCredential(ctx context.Context, candidate *store.Account) error {
	if !isGrokSSOAccount(candidate) {
		return nil
	}
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	token := grok.NormalizeSSOToken(candidate.ClientCookie)
	group := make([]*store.Account, 0, 2)
	for _, acc := range accounts {
		if isGrokSSOAccount(acc) && grok.NormalizeSSOToken(acc.ClientCookie) == token {
			group = append(group, acc)
		}
	}
	if len(group) == 0 {
		return nil
	}
	return a.ensureGrokSSOProviderGroup(ctx, group)
}

// deleteGrokSSOSourceAndLinkedConsoleAccounts removes the managed Console
// companions before its Web source. It is used both by explicit source deletion
// and by bounded compensation after failed pair provisioning.
func (a *API) deleteGrokSSOSourceAndLinkedConsoleAccounts(ctx context.Context, sourceID int64) error {
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		if isLinkedGrokConsoleAccount(acc) && acc.GrokSSOParentID == sourceID {
			if err := a.store.DeleteAccount(ctx, acc.ID); err != nil {
				return err
			}
		}
	}
	return a.store.DeleteAccount(ctx, sourceID)
}

// syncGrokSSOProviderView mirrors credential identity and source-owned
// scheduling from the visible Web source to its internal Console child. It
// deliberately preserves provider-specific Console runtime state.
func (a *API) syncGrokSSOProviderView(ctx context.Context, source *store.Account) error {
	found, err := grok.SyncWebSSOSource(ctx, a.store, source)
	if err != nil {
		return err
	}
	if found || !isGrokSSOAccount(source) || source.GrokSSOParentID != 0 || grok.ProviderForAccount(source) != grok.ProviderWeb {
		return nil
	}
	return a.EnsureGrokSSOProviderViews(ctx)
}

func (a *API) hasLinkedGrokConsoleCompanion(ctx context.Context, sourceID int64) (bool, error) {
	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return false, err
	}
	for _, acc := range accounts {
		if isLinkedGrokConsoleAccount(acc) && acc.GrokSSOParentID == sourceID {
			return true, nil
		}
	}
	return false, nil
}

func grokSSOViewsAreLinked(first, second *store.Account) bool {
	if !isGrokSSOAccount(first) || !isGrokSSOAccount(second) ||
		grok.NormalizeSSOToken(first.ClientCookie) != grok.NormalizeSSOToken(second.ClientCookie) {
		return false
	}
	return first.GrokSSOParentID == second.ID || second.GrokSSOParentID == first.ID
}
