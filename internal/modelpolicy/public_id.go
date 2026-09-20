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
