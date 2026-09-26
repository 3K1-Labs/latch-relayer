package watcher

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A half-open stream — headers sent, then nothing, forever — used to block the
// watcher's read with no way out. The watchdog must close it so Run reconnects.
func TestLiveness_ClosesSilentStream(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("retry: 1000\n"))
		w.(http.Flusher).Flush()
		<-release // then go silent
	}))
	// Deferred calls run last-first: release the handler before Close waits on it.
	defer srv.Close()
	defer close(release)

	var l liveness
	client := &http.Client{Transport: l.transport(http.DefaultTransport)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	heard := l.LastHeard()
	if time.Since(heard) > time.Second {
		t.Fatalf("LastHeard %v not advanced by the first line", heard)
	}

	// Not idle yet: the stream must be left alone.
	l.closeIfIdle(time.Hour, "pool")

	readErr := make(chan error, 1)
	go func() { _, err := r.ReadString('\n'); readErr <- err }()
	select {
	case err := <-readErr:
		t.Fatalf("read returned before the watchdog fired: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	l.closeIfIdle(0, "pool")
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("read succeeded on a closed stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not unblock the silent stream")
	}
	if !l.LastHeard().Equal(heard) {
		t.Fatal("closing the stream must not count as hearing from Horizon")
	}
}

// An error response is not proof Horizon is serving the stream.
func TestLiveness_ErrorResponseDoesNotCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var l liveness
	client := &http.Client{Transport: l.transport(http.DefaultTransport)}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !l.LastHeard().Equal(time.Unix(0, 0)) {
		t.Fatalf("LastHeard = %v after a 503", l.LastHeard())
	}
}
