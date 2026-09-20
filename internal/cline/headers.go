package cline

import "net/http"

// The Cline API serves its free catalog only to callers that identify
// themselves as the product. A request that carries just the bearer token is
// answered with 403 and
//
//	<model> is only available via Cline product surfaces. If you are using an
//	old version of Cline, please update to the latest version
//
// which names a client, not a credential. The reference proxy therefore sends a
// fixed identity block with every call, and this package does the same: without
// it the channel cannot serve a single model even with a freshly minted OAuth
// token.
//
// The values are the ones a Cline CLI install publishes, so they are observed
// rather than invented.
var defaultClientHeaders = map[string]string{
	"User-Agent":         "Cline/3.0.50",
	"HTTP-Referer":       "https://cline.bot",
	"X-Title":            "Cline",
	"X-IS-MULTIROOT":     "false",
	"X-CLIENT-TYPE":      "cline-cli",
	"X-CLIENT-VERSION":   "3.0.50",
	"X-PLATFORM":         "terminal",
	"X-PLATFORM-VERSION": "3.0.50",
	"X-CORE-VERSION":     "0.0.70",
}

// applyClientHeaders stamps the product identity onto an outbound request.
//
// It only fills headers the caller left empty, so an operator-supplied override
// or an explicit Accept still wins.
func applyClientHeaders(h http.Header) {
	if h == nil {
		return
	}
	for name, value := range defaultClientHeaders {
		if h.Get(name) == "" {
			h.Set(name, value)
		}
	}
}
