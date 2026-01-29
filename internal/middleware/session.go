package middleware

import (
	"crypto/sha256"
	"fmt"
	"net/http"
)

// SessionAuth checks for a session cookie or admin token
func SessionAuth(adminPass, adminToken string, next http.HandlerFunc) http.HandlerFunc {
	expectedSessionToken := fmt.Sprintf("%x", sha256.Sum256([]byte(adminPass)))
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. Check session cookie
		cookie, err := r.Cookie("session_token")
		if err == nil && cookie.Value == expectedSessionToken {
			next(w, r)
			return
		}

		// 2. Check Admin Token (Bearer or Header)
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

		// 3. Fallback to Basic Auth for API compatibility
		_, pass, ok := r.BasicAuth()
		if ok && pass == adminPass {
			next(w, r)
			return
		}

		// Unauthorized
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

// SessionAuthHandler is the same as SessionAuth but for http.Handler
func SessionAuthHandler(adminPass, adminToken string, next http.Handler) http.HandlerFunc {
	return SessionAuth(adminPass, adminToken, next.ServeHTTP)
}
