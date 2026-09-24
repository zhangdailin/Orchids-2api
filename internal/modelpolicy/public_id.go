package modelpolicy

import "strings"

// providerPublicPrefixes are the provider qualifiers a model name may carry.
// grok2api's NormalizePublicID strips them in any casing: the qualifier says
// which upstream plane serves the route, which is not part of the public model
// contract. An exact table entry always wins before this stripping is applied,
// so console/grok-4.5 keeps routing to the Console plane.
var providerPublicPrefixes = []string{"console/", "build/", "grok_console/", "grok_build/", "web/"}

// ExternalPublicID is the model name clients see.
func ExternalPublicID(internalID string) string {
	id := strings.ToLower(strings.TrimSpace(internalID))
	for _, prefix := range providerPublicPrefixes {
		if strings.HasPrefix(id, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(id, prefix))
		}
	}
	return id
}

// StripProviderPublicPrefix removes one provider qualifier, reporting whether it
// removed anything.
func StripProviderPublicPrefix(id string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(id))
	for _, prefix := range providerPublicPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(normalized, prefix)), true
		}
	}
	return normalized, false
}

// ProviderScopedPublicID puts the plane back on a bare public model name.
//
// Capability rules are provider-scoped: the Console plane's
// grok-4.20-0309-reasoning refuses an effort parameter while the same name on
// another plane accepts one, and "console/…" is what those rules key on. The
// public list publishes the bare name because the qualifier is a routing detail,
// so a caller that only holds the public name has to restore the plane before it
// asks a provider-scoped question — otherwise an alias can be advertised from
// the bare name and then rejected by the resolver, which asks with the plane.
func ProviderScopedPublicID(provider, publicID string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	publicID = strings.ToLower(strings.TrimSpace(publicID))
	if provider == "" || publicID == "" || strings.Contains(publicID, "/") {
		return publicID
	}
	switch provider {
	case "console", "web", "build":
		return provider + "/" + publicID
	}
	return publicID
}
