package grok

import (
	"net/http"
	"strings"
)

func (h *Handler) HandleAdminVerify(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, map[string]interface{}{
		"status": "ok",
	})
}

func (h *Handler) HandleAdminStorage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	storageType := "redis"
	if h != nil && h.configSnapshot() != nil && strings.TrimSpace(h.configSnapshot().StoreMode) != "" {
		storageType = strings.ToLower(strings.TrimSpace(h.configSnapshot().StoreMode))
	}
	writeJSON(w, map[string]interface{}{
		"type": storageType,
	})
}
