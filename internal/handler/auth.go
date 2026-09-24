package handler

import (
	"net/http"

	"github.com/latch/relayer/internal/httpx"
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
	return httpx.RequireBearer(h.config.APIKey)(next)
}
