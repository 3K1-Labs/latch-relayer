package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// RequireBearer rejects any request that does not present key as
// `Authorization: Bearer <key>`. /health stays open so orchestrators can probe
// liveness without the secret.
func RequireBearer(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			// Constant-time: a short-circuiting compare leaks how many leading
			// bytes the caller got right, enough to walk the secret byte by byte.
			if subtle.ConstantTimeCompare([]byte(BearerToken(r.Header.Get("Authorization"))), []byte(key)) != 1 {
				slog.Warn("auth: rejected request", "method", r.Method, "path", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BearerToken pulls the token out of an `Authorization: Bearer <token>`
// header, returning "" when the header is absent or malformed.
func BearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
