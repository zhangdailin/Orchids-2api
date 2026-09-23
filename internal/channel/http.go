package channel

import (
	"encoding/json"
	"net/http"
)

type RegistryPayload struct {
	Providers []Definition `json:"providers"`
	Default   ID           `json:"defaultProviderKey"`
}

func Payload() RegistryPayload {
	return RegistryPayload{Providers: All(), Default: Default().ID}
}

func HandleRegistry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Payload())
}
