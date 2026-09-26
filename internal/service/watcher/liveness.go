package watcher

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// streamIdleTimeout is how long a stream may go without a byte from Horizon
// before the watchdog drops it and reconnects.
//
// A new pool can see no deposits for months, so silence alone is normal. What
// is not normal is a connection that stopped delivering anything at all: a NAT
// or load balancer that forgot the connection leaves a half-open socket, and
// the SDK's read blocks on it forever — its request carries no context, so
// nothing else would ever end it. Reconnecting is cheap (it resumes from the
// saved cursor) and every successful connect is itself proof of life, so an
// idle pool simply reconnects on this interval.
const streamIdleTimeout = 2 * time.Minute

// StaleAfter is how long a watcher may go without hearing from Horizon before
// /health reports it. It spans a watchdog reconnect plus a couple of failed
// attempts, so it trips only when Horizon has stayed unreachable.
const StaleAfter = 5 * time.Minute

// liveness tracks the last time this watcher's stream received anything from
// Horizon — response headers or any body byte — and the body currently being
// read, so the watchdog can close it.
type liveness struct {
	lastHeard atomic.Int64 // unix nanoseconds

	mu   sync.Mutex
	body io.Closer
}

func (l *liveness) touch() { l.lastHeard.Store(time.Now().UnixNano()) }

// LastHeard reports when the stream last received anything from Horizon.
func (l *liveness) LastHeard() time.Time { return time.Unix(0, l.lastHeard.Load()) }

// transport wraps base so that every successful stream response, and every
// byte read from it, counts as hearing from Horizon.
func (l *liveness) transport(base http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
			return resp, err
		}
		l.touch()
		body := &touchingBody{ReadCloser: resp.Body, l: l}
		l.mu.Lock()
		l.body = body
		l.mu.Unlock()
		resp.Body = body
		return resp, nil
	})
}

// closeIfIdle closes the current stream body when nothing has arrived for
// idle, which makes the blocked read return and the watcher reconnect.
func (l *liveness) closeIfIdle(idle time.Duration, pool string) {
	silent := time.Since(l.LastHeard())
	if silent < idle {
		return
	}
	l.mu.Lock()
	body := l.body
	l.body = nil
	l.mu.Unlock()
	if body == nil {
		return // not connected; Run is already retrying
	}
	slog.Warn("watcher: stream silent, forcing reconnect", "pool", pool, "silent", silent.Round(time.Second))
	_ = body.Close()
}

type touchingBody struct {
	io.ReadCloser
	l *liveness
}

func (b *touchingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.l.touch()
	}
	return n, err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
