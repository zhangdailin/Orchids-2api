package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"github.com/goccy/go-json"

	"orchids-api/internal/auth"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/util"
)

const (
	APIKeyDenialExpired     = "api_key_expired"
	APIKeyDenialRateLimited = "rate_limit_exceeded"
)

type APIKeyPrincipal struct {
	ID            int64
	AllowedModels []string
	MaxConcurrent int
	DenialCode    string
}

type keyConcurrencyEntry struct {
	active int
}

// APIKeyConcurrencyWithTracker uses the deployment-wide Redis tracker when
// available, making the key limit atomic across replicas. Negative tracker IDs
// form a namespace disjoint from positive account IDs.
func APIKeyConcurrencyWithTracker(next http.HandlerFunc, tracker loadbalancer.ConnTracker) http.HandlerFunc {
	var mu sync.Mutex
	entries := make(map[int64]*keyConcurrencyEntry)
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := r.Context().Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
		if principal == nil || principal.ID == 0 || principal.MaxConcurrent <= 0 {
			next(w, r)
			return
		}
		if tracker != nil {
			trackerID := -principal.ID
			acquired := false
			if limiter, ok := tracker.(loadbalancer.LimitedConnTracker); ok {
				acquired = limiter.TryAcquire(trackerID, int64(principal.MaxConcurrent))
			} else if tracker.GetCount(trackerID) < int64(principal.MaxConcurrent) {
				tracker.Acquire(trackerID)
				acquired = true
			}
			if !acquired {
				w.Header().Set("Retry-After", "1")
				writeAPIKeyError(w, http.StatusTooManyRequests, "API key concurrency limit exceeded", "concurrency_limit_exceeded")
				return
			}
			defer tracker.Release(trackerID)
			next(w, r)
			return
		}
		mu.Lock()
		entry := entries[principal.ID]
		if entry == nil {
			entry = &keyConcurrencyEntry{}
			entries[principal.ID] = entry
		}
		if entry.active >= principal.MaxConcurrent {
			mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeAPIKeyError(w, http.StatusTooManyRequests, "API key concurrency limit exceeded", "concurrency_limit_exceeded")
			return
		}
		entry.active++
		mu.Unlock()
		defer func() {
			mu.Lock()
			entry.active--
			if entry.active <= 0 {
				delete(entries, principal.ID)
			}
			mu.Unlock()
		}()
		next(w, r)
	}
}

type APIKeyValidator func(context.Context, string) (*APIKeyPrincipal, error)

type apiKeyFingerprintContextKey struct{}
type apiKeyPrincipalContextKey struct{}

// APIKeyFingerprint returns a stable, non-secret identity for the validated
// client key. Stateful inference resources use it to enforce ownership.
func APIKeyFingerprint(ctx context.Context) string {
	value, _ := ctx.Value(apiKeyFingerprintContextKey{}).(string)
	return value
}

// APIKeyID returns the non-secret database identity of the validated client
// key for request audit and usage-ledger attribution.
func APIKeyID(ctx context.Context) int64 {
	principal, _ := ctx.Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
	if principal == nil {
		return 0
	}
	return principal.ID
}

// APIKeyAllowsModel checks the exact, case-insensitive model allowlist attached
// by APIKeyAuth. An absent or empty allowlist is unrestricted for compatibility.
func APIKeyAllowsModel(ctx context.Context, model string) bool {
	principal, _ := ctx.Value(apiKeyPrincipalContextKey{}).(*APIKeyPrincipal)
	if principal == nil || len(principal.AllowedModels) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, allowed := range principal.AllowedModels {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "*" || (model != "" && allowed == model) {
			return true
		}
	}
	return false
}

// APIKeyAuth protects model and inference endpoints with a managed key. It
// accepts OpenAI-style Bearer auth and Anthropic's x-api-key header. enabled is
// evaluated per request so config hot reloads take effect.
func APIKeyAuth(enabled func() bool, validate APIKeyValidator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if enabled != nil && !enabled() {
			ctx := context.WithValue(r.Context(), apiKeyFingerprintContextKey{}, "anonymous")
			next(w, r.WithContext(ctx))
			return
		}
		token := apiKeyToken(r)
		if token == "" {
			writeAPIKeyError(w, http.StatusUnauthorized, "Missing API key", "invalid_api_key")
			return
		}
		principal, err := validate(r.Context(), token)
		if err != nil {
			writeAPIKeyError(w, http.StatusServiceUnavailable, "API key validation unavailable", "auth_unavailable")
			return
		}
		if principal == nil {
			writeAPIKeyError(w, http.StatusUnauthorized, "Invalid API key", "invalid_api_key")
			return
		}
		switch principal.DenialCode {
		case APIKeyDenialExpired:
			writeAPIKeyError(w, http.StatusUnauthorized, "API key has expired", APIKeyDenialExpired)
			return
		case APIKeyDenialRateLimited:
			w.Header().Set("Retry-After", "60")
			writeAPIKeyError(w, http.StatusTooManyRequests, "API key rate limit exceeded", APIKeyDenialRateLimited)
			return
		}
		digest := sha256.Sum256([]byte(token))
		ctx := context.WithValue(r.Context(), apiKeyFingerprintContextKey{}, hex.EncodeToString(digest[:]))
		ctx = context.WithValue(ctx, apiKeyPrincipalContextKey{}, principal)
		next(w, r.WithContext(ctx))
	}
}

// WithAPIKeyPrincipalForTest attaches a validated principal to a context so tests
// can exercise allowlist-dependent handlers without running the auth middleware.
func WithAPIKeyPrincipalForTest(ctx context.Context, principal *APIKeyPrincipal) context.Context {
	return context.WithValue(ctx, apiKeyPrincipalContextKey{}, principal)
}

func writeAPIKeyError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	errorType := "authentication_error"
	if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errorType,
			"code":    code,
		},
	})
}

func bearerToken(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return ""
	}
	parts := strings.Fields(authHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func apiKeyToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func writeBearerUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"detail": message,
	})
}

func SessionAuthDynamic(credentials func() (adminPass, adminToken string), next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminPass, adminToken := credentials()

		cookie, err := r.Cookie("session_token")
		if err == nil && auth.ValidateSessionToken(cookie.Value) {
			next(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		adminHeader := r.Header.Get("X-Admin-Token")
		secrets := [...]string{adminToken, adminPass}
		for _, secret := range secrets {
			if secret != "" && (util.SecureCompare(authHeader, "Bearer "+secret) ||
				util.SecureCompare(authHeader, secret) || util.SecureCompare(adminHeader, secret)) {
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
			for _, secret := range secrets {
				if secret != "" && util.SecureCompare(queryKey, secret) {
					next(w, r)
					return
				}
			}
		}

		_, pass, ok := r.BasicAuth()
		if ok && util.SecureCompare(pass, adminPass) {
			next(w, r)
			return
		}

		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

func PublicKeyAuth(publicKey string, next http.HandlerFunc) http.HandlerFunc {
	key := strings.TrimSpace(publicKey)
	return func(w http.ResponseWriter, r *http.Request) {
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
		if !util.SecureCompare(token, key) {
			writeBearerUnauthorized(w, "Invalid authentication token")
			return
		}
		next(w, r)
	}
}

func PublicImagineStreamAuth(publicKey string, next http.HandlerFunc) http.HandlerFunc {
	key := strings.TrimSpace(publicKey)
	return func(w http.ResponseWriter, r *http.Request) {
		taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
		if taskID != "" {
			next(w, r)
			return
		}

		if key == "" {
			// Project override: empty public_key means no auth on public APIs.
			next(w, r)
			return
		}

		queryKey := strings.TrimSpace(r.URL.Query().Get("public_key"))
		if queryKey == "" {
			if token := bearerToken(r); util.SecureCompare(token, key) {
				next(w, r)
				return
			}
			writeBearerUnauthorized(w, "Missing authentication token")
			return
		}
		if !util.SecureCompare(queryKey, key) {
			writeBearerUnauthorized(w, "Invalid authentication token")
			return
		}
		next(w, r)
	}
}
