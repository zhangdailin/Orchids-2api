package handler

import (
	"context"
	"strings"

	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/warp"
	"orchids-api/internal/workbuddy"
)

// Model context windows.
//
// A client that cannot see a model's window falls back to its own default
// (pi-ai uses 262144, Codex uses its compiled-in table). The fallback is wrong in
// both directions: too small and a legitimate long session is reported as a
// context overflow after the model already answered, too large and the client
// keeps sending a history the upstream will refuse. The gateway already knows
// the real number — it forwards it upstream — so the public model list has to
// report the same observation.
//
// Nothing here is invented. A model whose window was never observed reports
// nothing at all, which is honest: the client then applies its own default rather
// than a number this process made up.

// modelContextWindows is the observed window per channel and model id.
type modelContextWindows struct {
	// input is keyed by contextKey(channel, model id).
	input map[string]int
	// output carries the declared output budget where the catalog published one.
	output map[string]int
}

func contextKey(channel, modelID string) string {
	return strings.ToLower(strings.TrimSpace(channel)) + "\x00" + strings.ToLower(strings.TrimSpace(modelID))
}

// lookup answers the input window and output budget for one model. A zero means
// "never observed" and must be rendered as an absent field, not as a zero.
func (w *modelContextWindows) lookup(channel string, modelIDs ...string) (int, int) {
	if w == nil {
		return 0, 0
	}
	for _, modelID := range modelIDs {
		if strings.TrimSpace(modelID) == "" {
			continue
		}
		key := contextKey(channel, modelID)
		input, hasInput := w.input[key]
		output, hasOutput := w.output[key]
		if hasInput || hasOutput {
			return input, output
		}
	}
	return 0, 0
}

// observedModelContextWindows reads every window this deployment has already
// observed from the upstream catalogs. The accounts are read once and shared, so
// a channel whose window table is not per-account (Warp publishes it with each
// model choice) is still filled from the same pass.
func (h *Handler) observedModelContextWindows(ctx context.Context) *modelContextWindows {
	w := &modelContextWindows{input: map[string]int{}, output: map[string]int{}}
	accounts, err := h.enabledAccountsForContextWindows(ctx)
	if err != nil {
		return w
	}

	// Warp: aggregate only snapshots belonging to currently enabled Warp
	// accounts. Disabled/deleted accounts must not keep stale context visible.
	if h != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
		activeWarpIDs := make(map[int64]struct{})
		for _, acc := range accounts {
			if acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") {
				activeWarpIDs[acc.ID] = struct{}{}
			}
		}
		if choices, err := warp.LoadAccountModelChoices(ctx, h.loadBalancer.Store); err == nil && choices != nil {
			for modelID, window := range warp.ContextWindowsForAccountIDs(choices, activeWarpIDs) {
				if window.Max > 0 {
					w.input[contextKey("warp", modelID)] = int(window.Max)
				}
			}
		}
	}
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(acc.AccountType)) {
		case "qoder":
			// The Qoder catalog declares max_input_tokens per model and the
			// request path already forwards it.
			mergeInputWindows(w, "qoder", qoder.CatalogContextWindows(acc.QoderModelIDs))
		case "cline":
			// The Cline feed names models, not windows, and the upstream caps
			// the request itself, so no window is recorded here.
		case "workbuddy":
			input, output := workbuddy.CatalogContextWindows(acc.WorkBuddyModelIDs)
			mergeInputWindows(w, "workbuddy", input)
			for modelID, limit := range output {
				key := contextKey("workbuddy", modelID)
				if existing, ok := w.output[key]; !ok || limit > existing {
					w.output[key] = limit
				}
			}
		}
	}

	// Grok is deliberately absent here: its window is not account-scoped and is
	// already published by the static table the Codex catalog uses, so
	// modelContextWindow reads it from that same table. Adding it to these maps
	// would be a second copy that could drift.
	return w
}

func (h *Handler) enabledAccountsForContextWindows(ctx context.Context) ([]*store.Account, error) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return nil, nil
	}
	return h.loadBalancer.Store.GetEnabledAccounts(ctx)
}

func mergeInputWindows(w *modelContextWindows, channel string, windows map[string]int) {
	for modelID, limit := range windows {
		if limit <= 0 {
			continue
		}
		key := contextKey(channel, modelID)
		if existing, ok := w.input[key]; !ok || limit > existing {
			w.input[key] = limit
		}
	}
}

// grokDeclaredContextWindow is the Grok window from the catalog's own table. It
// is a separate path because Grok's window is not account-scoped and is already
// published to Codex-family clients.
func grokDeclaredContextWindow(modelID string) int {
	slug := modelpolicy.GrokModelSlug(modelID)
	if metadata, ok := codexModelMetadataTable[slug]; ok {
		return metadata.contextWindow
	}
	return 0
}

// modelContextWindow resolves the reported window for one route row. It answers
// zero when nothing was observed, and the caller omits the field.
func (h *Handler) modelContextWindow(ctx context.Context, windows *modelContextWindows, channel, modelID, publicID string) (int, int) {
	if strings.EqualFold(strings.TrimSpace(channel), "grok") {
		declared := grokDeclaredContextWindow(modelID)
		if declared == 0 {
			declared = grokDeclaredContextWindow(publicID)
		}
		if declared > 0 {
			return declared, 0
		}
	}
	return windows.lookup(channel, modelID, publicID)
}
