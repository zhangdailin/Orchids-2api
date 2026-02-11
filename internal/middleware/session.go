package middleware

import (
	"net/http"
	"orchids-api/internal/auth"
)

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

		_, pass, ok := r.BasicAuth()
		if ok && pass == adminPass {
			next(w, r)
			return
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}
