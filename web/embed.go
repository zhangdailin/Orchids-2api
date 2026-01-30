package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/*
var staticFS embed.FS

//go:embed templates/*
var TemplateFS embed.FS

func StaticHandler() http.Handler {
	subFS, _ := fs.Sub(staticFS, "static")
	return http.FileServer(http.FS(subFS))
}
