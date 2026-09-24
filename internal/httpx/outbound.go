package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"time"
)

// OutboundTransport is the connection pool for calls to Horizon / Stellar RPC.
// OZ constants: connect 2s, keep-alive 30s. The stdlib default keeps only 2
// idle connections per host, so under concurrent load every extra call would
// open a fresh TCP+TLS connection.
func OutboundTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
}

// CallerKey identifies the caller for rate limiting. It runs after auth, so
// the bearer token is always a valid key here; hashing keeps the secret itself
// out of the limiter's map.
func CallerKey(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:8])
}
