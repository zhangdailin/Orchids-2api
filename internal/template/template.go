package template

import (
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"orchids-api/internal/config"
	"orchids-api/web"
)

// Renderer handles template rendering
type Renderer struct {
	templates *template.Template
	mu        sync.RWMutex
}

// NewRenderer creates a new template renderer
func NewRenderer() (*Renderer, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	return &Renderer{
		templates: tmpl,
	}, nil
}

// parseTemplates parses all template files from the embedded filesystem
func parseTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"formatDate": formatDate,
		"maskToken":  maskToken,
		"add":        add,
		"sub":        sub,
		"mul":        mul,
		"div":        div,
		"mod":        mod,
		"eq":         eq,
		"ne":         ne,
		"lt":         lt,
		"le":         le,
		"gt":         gt,
		"ge":         ge,
	}

	tmpl := template.New("").Funcs(funcMap)

	// Parse all templates recursively
	err := fs.WalkDir(web.TemplateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}

		content, err := fs.ReadFile(web.TemplateFS, path)
		if err != nil {
			return err
		}

		// Use the filename without extension as template name
		name := filepath.Base(path)
		_, err = tmpl.New(name).Parse(string(content))
		return err
	})

	return tmpl, err
}

// RenderIndex renders the main index page
func (r *Renderer) RenderIndex(w http.ResponseWriter, req *http.Request, cfg *config.Config) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	activeTab := getActiveTab(req)

	data := &PageData{
		Title:     "API 管理面板",
		AdminPath: cfg.AdminPath,
		ActiveTab: activeTab,
		Stats: &Stats{
			TotalAccounts:    0,
			NormalAccounts:   0,
			AbnormalAccounts: 0,
			SelectedAccounts: 0,
		},
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Render different page templates based on active tab
	var templateName string
	switch activeTab {
	case "tutorial":
		templateName = "page-tutorial"
	case "models":
		templateName = "page-models"
	case "keys":
		templateName = "page-config"
	case "accounts":
		templateName = "page-accounts"
	default:
		templateName = "page-accounts"
	}

	return r.templates.ExecuteTemplate(w, templateName, data)
}

// getActiveTab extracts the active tab from the request
func getActiveTab(req *http.Request) string {
	tab := req.URL.Query().Get("tab")
	if tab == "" {
		tab = "accounts"
	}
	return tab
}
