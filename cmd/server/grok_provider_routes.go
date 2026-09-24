package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Grok's public model rows have two different authorities, and publishing only
// one of them is what left the Web and Console planes with nothing to route.
//
//   - Build is an *upstream observation*. discoverGrokModelsReport reads the
//     account's own capability catalog, so its rows are exactly what a live
//     account advertised and a withdrawn model disappears with the next read.
//   - Web (AppChat) and Console have no model catalog read at all: neither
//     account type publishes a model list, and grok2api serves both planes from
//     compiled-in route tables (provider/web/catalog.go, provider/console/
//     catalog.go). Without an equivalent row here, every entry the compatibility
//     table declares for those planes is unreachable — the accounts are enabled,
//     healthy and selected by nobody, while the catalog looks complete because
//     it is full of Build models.
//
// A plane route is therefore the gateway's own contract. It is seeded only for a
// plane that has at least one enabled account, so a deployment with no Web or
// Console credential never advertises those models, and it is seeded before the
// upstream catalog read so a failed (or absent) Build read cannot suppress it.
//
// Rows are placed after the discovered ones in the list order: the tools page
// takes the first chat model as its default, and publishing a new plane must not
// silently change which model that is.
const grokProviderRouteSortBase = 1000

const grokProviderRouteSource = "grok_provider_routes"

// grokProviderRoutePlanes are the planes whose route table the gateway owns. The
// provider name is what account selection filters on, and the upstream kind is
// what the compatibility table uses to describe the plane.
var grokProviderRoutePlanes = []struct {
	provider string
	upstream grok.UpstreamKind
}{
	{provider: grok.ProviderWeb, upstream: grok.UpstreamAppChat},
	{provider: grok.ProviderConsole, upstream: grok.UpstreamConsole},
}

// ensureGrokProviderRouteModels publishes the route rows the Web and Console
// planes are missing, and returns the identifiers it created.
//
// Seeding is creation-only: a row that already exists — whatever its status,
// origin or plane — is never rewritten. An operator disables a route to hide it,
// and the next refresh must not undo that; a row that needs correcting is the
// operator's or the Build discovery's to own.
func ensureGrokProviderRouteModels(ctx context.Context, s *store.Store) ([]string, error) {
	if s == nil {
		return nil, errors.New("store not configured")
	}
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	planes := map[string]int{}
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
			continue
		}
		if provider := grok.ProviderForAccount(acc); provider != "" {
			planes[provider]++
		}
	}
	if len(planes) == 0 {
		return nil, nil
	}

	existing, err := grokChannelModelIDs(ctx, s)
	if err != nil {
		return nil, err
	}

	added := make([]string, 0, 8)
	for index, plane := range grokProviderRoutePlanes {
		if planes[plane.provider] == 0 {
			continue
		}
		sortBase := grokProviderRouteSortBase + index*100
		for offset, spec := range grok.SupportedModels {
			// Only conversation routes are published here. Media, voice and STT
			// rows have their own lifecycle (tier gating, asset billing) and are
			// not what an idle SSO account pool needs to become reachable.
			if spec.Upstream != plane.upstream || !spec.SupportsConversation() {
				continue
			}
			if grok.IsDeprecatedModelID(spec.ID) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(spec.ID))
			if _, exists := existing[key]; exists {
				continue
			}
			row := &store.Model{
				Channel:   "grok",
				ModelID:   spec.ID,
				Name:      util.FirstNonEmpty(spec.Name, spec.ID),
				Status:    store.ModelStatusAvailable,
				SortOrder: sortBase + offset,
				// Verified is what makes a Grok row routable
				// (modelpolicy.IsVisibleGrokModel), and this row is verified by a
				// different evidence than a discovered one: the plane is live,
				// because an enabled account of that provider exists, and the
				// route table is the gateway's contract for it. Publishing it
				// unverified would create a row that is listed but cannot be
				// served — the exact failure this function removes.
				Verified: true,
			}
			store.ApplyGrokRouteDefaults(row)
			if !strings.EqualFold(row.Provider, plane.provider) {
				// The compatibility table and the route defaults disagree about
				// the plane. Routing reads the defaults, so publishing the row
				// under the table's plane would advertise a route nothing selects.
				slog.Warn("Grok provider route has no matching plane",
					"model", spec.ID, "plane", plane.provider, "derived", row.Provider)
				continue
			}
			if err := s.CreateModel(ctx, row); err != nil {
				return added, fmt.Errorf("publish grok %s route %s: %w", plane.provider, spec.ID, err)
			}
			existing[key] = struct{}{}
			added = append(added, spec.ID)
		}
	}
	if len(added) > 0 {
		slog.Info("Published Grok provider routes", "models", strings.Join(added, ","))
	}
	return added, nil
}

// grokChannelModelIDs indexes the Grok rows that already exist, so the seeding
// pass costs one read instead of one full scan per candidate.
func grokChannelModelIDs(ctx context.Context, s *store.Store) (map[string]struct{}, error) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "grok") {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(model.ModelID))] = struct{}{}
	}
	return out, nil
}
