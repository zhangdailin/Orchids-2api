package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*
var TemplateFS embed.FS

func StaticHandler() http.Handler {
	subFS, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(subFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ext := strings.ToLower(path.Ext(r.URL.Path))
		versioned := strings.TrimSpace(r.URL.Query().Get("v")) != ""
		switch {
		case versioned && isImmutableAsset(ext):
			// Asset URLs carry an explicit version query in every page template.
			// Let browsers retain those immutable bytes instead of downloading the
			// shared stylesheet and scripts on every admin navigation.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// HTML entry points and unversioned assets must continue to pick up a
			// deployment that changes their content at the same URL.
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func isImmutableAsset(ext string) bool {
	switch ext {
	case ".css", ".js", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".gif", ".ico", ".woff", ".woff2":
		return true
	default:
		return false
	}
}
