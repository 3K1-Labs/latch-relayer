package handler

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// RequireAPIKey rejects any request that does not present the relayer's shared
// secret as `Authorization: Bearer <key>`.
//
// The relayer mints intents for arbitrary C-addresses and serves the
// memo_id → C-address mapping, so an unauthenticated deployment lets anyone
// enumerate deposits or point a funding session wherever they like. latch-api is
// the only intended caller.
//
// /health stays open so orchestrators can probe liveness without the secret; it
// reveals nothing but DB reachability.
func (h *Handler) RequireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		// Constant-time: a short-circuiting compare leaks how many leading bytes
		// the caller got right, which is enough to walk the secret one byte at a time.
		key := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(key), []byte(h.config.APIKey)) != 1 {
			slog.Warn("auth: rejected request", "method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// bearerToken pulls the token out of an `Authorization: Bearer <token>` header,
// returning "" when the header is absent or malformed.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
