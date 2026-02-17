package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	"orchids-api/internal/auth"
)

func bearerToken(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(authHeader, prefix))
}

func writeBearerUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"detail": message,
	})
}

func SessionAuth(adminPass, adminToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session_token")
		if err == nil && auth.ValidateSessionToken(cookie.Value) {
			next(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if adminToken != "" {
			if authHeader == "Bearer "+adminToken || authHeader == adminToken {
				next(w, r)
				return
			}
			if r.Header.Get("X-Admin-Token") == adminToken {
				next(w, r)
				return
			}
		}
		if adminPass != "" {
			if authHeader == "Bearer "+adminPass || authHeader == adminPass {
				next(w, r)
				return
			}
			if r.Header.Get("X-Admin-Token") == adminPass {
				next(w, r)
				return
			}
		}

		queryKeys := []string{
			strings.TrimSpace(r.URL.Query().Get("app_key")),
			strings.TrimSpace(r.URL.Query().Get("public_key")),
		}
		for _, queryKey := range queryKeys {
			if queryKey == "" {
				continue
			}
			if adminToken != "" && queryKey == adminToken {
				next(w, r)
				return
			}
			if adminPass != "" && queryKey == adminPass {
				next(w, r)
				return
			}
		}

		_, pass, ok := r.BasicAuth()
		if ok && pass == adminPass {
			next(w, r)
			return
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

func PublicKeyAuth(publicKey string, _ bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(publicKey)
		if key == "" {
			// Project override: empty public_key means no auth on public APIs.
			next(w, r)
			return
		}

		token := bearerToken(r)
		if token == "" {
			writeBearerUnauthorized(w, "Missing authentication token")
			return
		}
		if token != key {
			writeBearerUnauthorized(w, "Invalid authentication token")
			return
		}
		next(w, r)
	}
}

func PublicImagineStreamAuth(publicKey string, _ bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
		if taskID != "" {
			next(w, r)
			return
		}

		key := strings.TrimSpace(publicKey)
		if key == "" {
			// Project override: empty public_key means no auth on public APIs.
			next(w, r)
			return
		}

		queryKey := strings.TrimSpace(r.URL.Query().Get("public_key"))
		if queryKey == "" {
			if token := bearerToken(r); token == key {
				next(w, r)
				return
			}
			writeBearerUnauthorized(w, "Missing authentication token")
			return
		}
		if queryKey != key {
			writeBearerUnauthorized(w, "Invalid authentication token")
			return
		}
		next(w, r)
	}
}
