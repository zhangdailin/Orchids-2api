package api

import (
	"io"
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"orchids-api/internal/debug"
)

func (a *API) SetDiagnosticStore(s *debug.DiagnosticStore) { a.diagnostics = s }

func (a *API) HandleDiagnosticSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if r.Method == http.MethodPut {
		var input struct {
			Enabled *bool `json:"enabled"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || input.Enabled == nil {
			http.Error(w, "enabled must be a boolean", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		current := a.config.Load()
		if current == nil {
			http.Error(w, "Configuration unavailable", http.StatusServiceUnavailable)
			return
		}
		next := current.Clone()
		next.DebugEnabled = *input.Enabled
		if err := a.persistConfig(r.Context(), current, next); err != nil {
			http.Error(w, "Could not save diagnostic setting", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": a.DiagnosticsEnabled()})
}
func (a *API) DiagnosticsEnabled() bool {
	cfg := a.config.Load()
	return cfg != nil && cfg.DebugEnabled
}
func (a *API) HandleJournalDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("request_id"))
	if id == "" || len(id) > 512 {
		http.Error(w, "Invalid request_id", http.StatusBadRequest)
		return
	}
	bundle, err := a.diagnostics.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "Could not read diagnostics", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]interface{}{"available": bundle != nil, "retention": "24 小时，最多 512 个请求"}
	if bundle != nil {
		payload["entry"] = bundle
	} else {
		payload["note"] = "该请求没有保留的诊断内容（可能在采集启用前产生，或已超过保留期限）。"
	}
	_ = json.NewEncoder(w).Encode(payload)
}
