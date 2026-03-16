package api

import (
	"fmt"
	"net/http"

	"github.com/goccy/go-json"
)

// HandleTokenCacheStats handles GET /api/token-cache/stats
func (a *API) HandleTokenCacheStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if a.promptCache == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{
				"key_count":       0,
				"memory_used_str": "0 B",
				"connected":       false,
			},
		})
		return
	}

	count, memBytes, _ := a.promptCache.GetStats(r.Context())
	json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{
			"key_count":       count,
			"memory_used_str": fmt.Sprintf("%.2f MB", float64(memBytes)/1024/1024),
			"connected":       true,
		},
	})
}

// HandleTokenCacheClear handles POST /api/token-cache/clear
func (a *API) HandleTokenCacheClear(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if a.promptCache == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"message": "Token cache is not enabled or not properly initialized",
		})
		return
	}

	if err := a.promptCache.Clear(r.Context()); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 1,
			"message": "Failed to clear token cache: " + err.Error(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"code": 0,
		"message": "Success",
	})
}
