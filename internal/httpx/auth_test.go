package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
		if got := BearerToken(tc.header); got != tc.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestRequireBearer(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	h := RequireBearer(key)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, tc := range []struct {
		path, auth string
		want       int
	}{
		{"/gasless/quote", "Bearer " + key, http.StatusOK},
		{"/gasless/quote", "Bearer wrong", http.StatusUnauthorized},
		{"/gasless/quote", "", http.StatusUnauthorized},
		{"/metrics", "", http.StatusUnauthorized},
		{"/health", "", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s with %q: status %d, want %d", tc.path, tc.auth, rec.Code, tc.want)
		}
	}
}
