// Package provider maps an account type to the upstream client that serves it.
//
// Every channel builds its client the same way — NewFromAccount(acc, cfg) — and
// every one of them satisfies handler.UpstreamClient once built, so a provider is
// a name and a constructor and nothing else. The table below is the whole
// mapping. It used to be spread over four files, each declaring an empty struct
// with the same two methods over a different constructor, plus an interface and a
// registry to hold them; the indirection only moved the type switch it was meant
// to remove, and no caller ever added a provider at runtime.
package provider

import (
	"strings"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/workbuddy"
)

// Factory builds the upstream client for one account. The result is asserted to
// handler.UpstreamClient by the caller, which is the only thing it can be.
type Factory func(acc *store.Account, cfg *config.Config) interface{}

// factories maps an account type to its client constructor. Grok is absent on
// purpose: it has its own handler and is never built through this seam.
//
// Each entry is a closure because a constructor returns its own concrete client
// type and Go will not widen that on assignment; the closure is the single line
// that would otherwise be a whole file.
var factories = map[string]Factory{
	"workbuddy": func(acc *store.Account, cfg *config.Config) interface{} {
		return workbuddy.NewFromAccount(acc, cfg)
	},
	"qoder": func(acc *store.Account, cfg *config.Config) interface{} {
		return qoder.NewFromAccount(acc, cfg)
	},
	"cline": func(acc *store.Account, cfg *config.Config) interface{} {
		return cline.NewFromAccount(acc, cfg)
	},
}

// Get returns the client constructor for an account type.
//
// The lookup is case-insensitive because account types are normalized to lower
// case when stored but reach this seam from an operator, an import file, or a
// URL path in whatever case they were written.
func Get(accountType string) (Factory, bool) {
	factory, ok := factories[strings.ToLower(strings.TrimSpace(accountType))]
	return factory, ok
}
