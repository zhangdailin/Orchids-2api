package api

// TEMPORARY SCAFFOLDING (remove with the Web/Console account management below).
//
// The Grok Web SSO and Console planes were deleted from internal/grok: the only
// plane it serves now is ProviderBuild. This package still contains the legacy
// SSO/Console account-management surface (SSO views, companion linking, SSO
// credential normalisation), which is being retired next; until it is gone it
// needs the legacy provider labels to keep compiling. Nothing here routes a
// request: internal/grok.ProviderForAccount already reports Build for every Grok
// account, and the model table is Build-only.
const (
	grokProviderWeb     = "web"
	grokProviderConsole = "console"
)
