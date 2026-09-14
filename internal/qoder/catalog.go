package qoder

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"orchids-api/internal/store"
)

// The catalog is local, and that is a property of the protocol rather than a
// shortcut.
//
// The Qoder CLI's entire HTTP surface is four endpoints — the device token
// refresh, the user profile, the PAT job-token exchange and the chat SSE. It
// never reads a model list over the network: it carries a built-in catalog and
// optionally a locally cached `catalog-v6` blob. The OAuth device credential is
// therefore not accepted by the gateway's `/algo/api/v2/model/list`, which
// answers `403 code=101 Signature invalid` for a signature that is otherwise
// proven good by the chat call succeeding.
//
// Verified against a live account: a chat request with the same credential and
// the same runtime pair is authenticated (the gateway answers a business error
// about a missing subscription), while the model-list read is refused. Reading
// that endpoint would therefore turn a working credential into a bogus
// signature failure, so it is not read at all.
//
//   - The built-in catalog is the authority this channel can actually observe.
//     The account snapshot records whatever was installed, and the chat call —
//     not this list — is what reports an entitlement problem.
//   - CatalogFromSnapshot rebuilds the list from an account record, so an
//     operator-supplied snapshot keeps working without a network read.

// modelEntry is one catalog row. Key is the internal gateway key; Name is what a
// client may ask for.
type modelEntry struct {
	Key            string   `json:"key"`
	Name           string   `json:"name"`
	DisplayName    string   `json:"display_name"`
	Format         string   `json:"format"`
	Source         string   `json:"source"`
	Enable         *bool    `json:"enable"`
	IsVL           bool     `json:"is_vl"`
	IsReasoning    bool     `json:"is_reasoning"`
	IsDefault      bool     `json:"is_default"`
	PriceFactor    float64  `json:"price_factor"`
	MaxInputTokens int      `json:"max_input_tokens"`
	OrgTags        []string `json:"organization_tags"`
}

// enabled reports whether the gateway currently serves this row. A missing flag
// means enabled: the field is an opt-out, and treating absence as disabled would
// hide every model from a deployment that omits it.
func (m modelEntry) enabled() bool {
	return m.Enable == nil || *m.Enable
}

// catalogConfig is the nested configuration the gateway sends alongside a model
// row. It is kept so a catalog snapshot can be round-tripped without losing
// fields this channel does not read yet.
type catalogConfig struct {
	ContextConfig map[string]struct {
		TokenCount int `json:"token_count"`
	} `json:"context_config,omitempty"`
}

// Catalog is a resolved model catalog.
type Catalog struct {
	// entries are ordered as the upstream returned them.
	entries []modelEntry
	byKey   map[string]modelEntry
	byName  map[string]modelEntry
}

// Len reports how many models the catalog carries.
func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Entries returns the catalog rows in upstream order.
func (c *Catalog) Entries() []modelEntry {
	if c == nil {
		return nil
	}
	return append([]modelEntry(nil), c.entries...)
}

// Resolve maps a client-facing model name onto a catalog row.
//
// Both the internal key and the display name are accepted. A client that copies
// a key out of this gateway's own /v1/models response must keep working, and
// operators reasonably paste either form.
func (c *Catalog) Resolve(requested string) (modelEntry, error) {
	if c == nil || len(c.entries) == 0 {
		c = DefaultCatalog()
	}
	name := strings.TrimSpace(requested)
	if name == "" {
		return c.defaultEntry(), nil
	}
	if entry, ok := c.byKey[name]; ok {
		return entry, nil
	}
	if entry, ok := c.byName[name]; ok {
		return entry, nil
	}
	// Case-insensitive fallback: model names are matched case-insensitively by
	// the upstream, and a client that lowercased a display name should not be
	// told the model does not exist.
	lowered := strings.ToLower(name)
	for _, entry := range c.entries {
		if strings.ToLower(entry.Key) == lowered || strings.ToLower(entry.Name) == lowered {
			return entry, nil
		}
	}
	return modelEntry{}, fmt.Errorf("unsupported qoder model %q; available: %s", requested, c.SupportedList())
}

func (c *Catalog) defaultEntry() modelEntry {
	for _, entry := range c.entries {
		if entry.IsDefault {
			return entry
		}
	}
	// Prefer the cheapest capable tier rather than the flagship, so an empty
	// model name never silently routes every request to the most expensive one.
	for _, needle := range []string{"flash", "lite", "efficient", "plus"} {
		for _, entry := range c.entries {
			if strings.Contains(strings.ToLower(entry.Name), needle) {
				return entry
			}
		}
	}
	return c.entries[0]
}

// SupportedList renders the catalog for an error message.
func (c *Catalog) SupportedList() string {
	if c == nil {
		return ""
	}
	names := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Names returns the sorted client-facing model names.
func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return names
}

func newCatalog(entries []modelEntry) *Catalog {
	catalog := &Catalog{
		byKey:  make(map[string]modelEntry, len(entries)),
		byName: make(map[string]modelEntry, len(entries)),
	}
	for _, entry := range entries {
		if !entry.enabled() {
			continue
		}
		key := strings.TrimSpace(entry.Key)
		if key == "" || key == "auto" {
			// `auto` is a routing directive, not a runnable model.
			continue
		}
		// The wire uses display_name; the stored snapshot and the in-process
		// form use name. Either may be absent, so the key is the last resort.
		if strings.TrimSpace(entry.Name) == "" {
			entry.Name = strings.TrimSpace(entry.DisplayName)
		}
		if strings.TrimSpace(entry.Name) == "" {
			entry.Name = key
		}
		if entry.Format == "" {
			entry.Format = "openai"
		}
		if entry.Source == "" {
			entry.Source = "system"
		}
		catalog.entries = append(catalog.entries, entry)
		catalog.byKey[key] = entry
		if _, taken := catalog.byName[entry.Name]; !taken {
			catalog.byName[entry.Name] = entry
		}
	}
	return catalog
}

// catalogFromIDs rebuilds a catalog from an account's stored snapshot. The
// snapshot stores "<key>\t<display name>" so a display name that differs from
// the key round-trips, and a bare key (a hand-edited or imported snapshot) still
// resolves.
func catalogFromIDs(ids []string) *Catalog {
	entries := make([]modelEntry, 0, len(ids))
	for _, id := range ids {
		key, name, _ := strings.Cut(strings.TrimSpace(id), "\t")
		if strings.TrimSpace(key) == "" {
			continue
		}
		entries = append(entries, modelEntry{Key: strings.TrimSpace(key), Name: strings.TrimSpace(name)})
	}
	return newCatalog(entries)
}

// catalogToIDs projects a catalog onto the stored snapshot form.
func catalogToIDs(catalog *Catalog) []string {
	if catalog == nil {
		return nil
	}
	ids := make([]string, 0, len(catalog.entries))
	for _, entry := range catalog.entries {
		if strings.TrimSpace(entry.Name) == "" || entry.Name == entry.Key {
			ids = append(ids, entry.Key)
			continue
		}
		ids = append(ids, entry.Key+"\t"+entry.Name)
	}
	return ids
}

// seedModels is the built-in fallback catalog. It mirrors the CLI's own built-in
// list; actual availability always follows the account's fetched snapshot.
func seedModels() []modelEntry {
	specs := []struct {
		key, name string
		reasoning bool
		maxInput  int
	}{
		{"auto", "Auto", false, 180000},
		{"ultimate", "Ultimate", true, 1000000},
		{"performance", "Performance", false, 1000000},
		{"efficient", "Efficient", false, 180000},
		{"lite", "Lite", false, 180000},
		{"cmodel", "Cantus", true, 1000000},
		{"qmodel_38max", "Qwen3.8-Max", true, 1000000},
		{"qmodel_latest", "Qwen3.7-Max", false, 1000000},
		{"qmodel", "Qwen3.7-Plus", false, 1000000},
		{"kmodel_latest", "Kimi-K3", false, 1000000},
		{"kmodel", "Kimi-K2.7-Code", false, 256000},
		{"gmodel", "GLM-5.3", true, 1000000},
		{"gm51model", "GLM-5.2", true, 1000000},
		{"dmodel", "DeepSeek-V4-Pro", true, 1000000},
		{"dfmodel", "DeepSeek-V4-Flash", true, 1000000},
		{"mmodel", "MiniMax-M3", false, 1000000},
	}
	enabled := true
	entries := make([]modelEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, modelEntry{
			Key:            spec.key,
			Name:           spec.name,
			Format:         "openai",
			Source:         "system",
			Enable:         &enabled,
			IsVL:           true,
			IsReasoning:    spec.reasoning,
			IsDefault:      spec.key == "auto",
			PriceFactor:    1,
			MaxInputTokens: spec.maxInput,
		})
	}
	return entries
}

// DefaultCatalog returns the built-in fallback catalog.
func DefaultCatalog() *Catalog {
	return newCatalog(seedModels())
}

// FetchModels returns the catalog this channel can serve.
//
// It is local by construction: the OAuth device credential is not accepted by
// the gateway's model-list endpoint, so there is nothing to fetch over the
// network. Keeping one entry point means a future gateway that does expose an
// authenticated catalog changes exactly one place.
//
// The account snapshot wins when one is stored; otherwise the built-in list is
// returned. The error is always nil — the chat call, not this list, is what
// reports an entitlement problem.
func (c *Client) FetchModels(ctx context.Context) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account != nil {
		if catalog := catalogFromIDs(c.account.QoderModelIDs); catalog.Len() > 0 {
			return catalog, nil
		}
	}
	return DefaultCatalog(), nil
}

// FetchModelsLenient is the login-time entry point. It matches FetchModels and
// always succeeds, so a successfully issued device credential is never discarded
// over a catalog read.
func (c *Client) FetchModelsLenient(ctx context.Context) (*Catalog, error) {
	return c.FetchModels(ctx)
}

// SyncCatalog installs the catalog snapshot on the account.
func (c *Client) SyncCatalog(ctx context.Context) (*Catalog, error) {
	catalog, err := c.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := catalogToIDs(catalog)
	if err := c.persistPatch(ctx, store.QoderAccountPatch{ModelIDs: ids}); err != nil {
		return nil, err
	}
	c.stateMu.Lock()
	if c.account != nil {
		c.account.QoderModelIDs = append([]string(nil), ids...)
	}
	c.stateMu.Unlock()
	return catalog, nil
}
