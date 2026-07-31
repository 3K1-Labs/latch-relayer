package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/latch/relayer/internal/config"
)

const testAPIKey = "0123456789abcdef0123456789abcdef"

// authTestHandler returns a Handler wired with only the config the middleware
// touches. The store is nil: RequireAPIKey must reject before anything reaches it.
func authTestHandler() *Handler {
	return New(nil, &config.Config{APIKey: testAPIKey})
}

func TestRequireAPIKey(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "valid key passes through",
			path:       "/intents",
			authHeader: "Bearer " + testAPIKey,
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "no header rejected",
			path:       "/intents",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong key rejected",
			path:       "/intents",
			authHeader: "Bearer wrong-key-wrong-key-wrong-key",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "correct prefix but truncated is rejected",
			path:       "/intents",
			authHeader: "Bearer " + testAPIKey[:16],
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "raw key without the Bearer scheme is rejected",
			path:       "/intents",
			authHeader: testAPIKey,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "status endpoint is protected too",
			path:       "/deposit/status/123",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "health probe needs no key",
			path:       "/health",
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()

			authTestHandler().RequireAPIKey(next).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Errorf("next called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct{ header, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"}, // scheme is case-insensitive per RFC 7235
		{"Bearer  abc ", "abc"},
		{"", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"Basic abc", ""},
		{"abc", ""},
	}

	for _, tc := range cases {
		if got := bearerToken(tc.header); got != tc.want {
			t.Errorf("bearerToken(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
