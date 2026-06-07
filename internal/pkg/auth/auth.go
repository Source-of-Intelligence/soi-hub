// Package auth provides API key authentication for the skill-market service.
package auth

import (
	"net/http"
	"strings"
)

// Middleware returns an HTTP middleware that enforces API key authentication
// for write operations (POST, PUT, DELETE, PATCH).
// Read operations (GET, HEAD, OPTIONS) are always allowed.
func Middleware(apiKeys []string) func(http.Handler) http.Handler {
	if len(apiKeys) == 0 {
		// No keys configured -- allow all requests
		return func(next http.Handler) http.Handler {
			return next
		}
	}
	keySet := make(map[string]struct{}, len(apiKeys))
	for _, k := range apiKeys {
		keySet[strings.TrimSpace(k)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Allow read operations without auth
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			// Check API key
			key := strings.TrimSpace(r.Header.Get("X-API-Key"))
			if key == "" {
				key = strings.TrimSpace(r.URL.Query().Get("api_key"))
			}
			if _, ok := keySet[key]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"ok":false,"error":"unauthorized: valid X-API-Key header or api_key query param required","code":"UNAUTHORIZED"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
