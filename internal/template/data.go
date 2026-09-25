package template

// PageData represents the data passed to page templates
type PageData struct {
	Title     string
	AdminPath string
	ActiveTab string
	// AssetVersion is appended to every stylesheet and script URL as ?v=. It is a
	// content hash of the embedded assets (web.AssetVersion), so a build that
	// changes an asset also changes its URL and cannot be served from a browser
	// or CDN cache filled by an earlier deployment.
	AssetVersion string
}
