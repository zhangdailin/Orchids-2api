package modelpolicy

import "strings"

// providerPublicPrefixes are the accepted Build qualifiers.
var providerPublicPrefixes = []string{"build/", "grok_build/"}

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
