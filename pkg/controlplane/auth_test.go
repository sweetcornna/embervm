package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc123", "abc123", true},
		{"bearer abc123", "abc123", true},
		{"BEARER xyz", "xyz", true},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
		{"", "", false},
		{"abc123", "", false},
	}
	for _, tc := range cases {
		got, ok := bearerToken(tc.header)
		if got != tc.want || ok != tc.ok {
			t.Errorf("bearerToken(%q) = (%q,%v), want (%q,%v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := NewTokenStore(map[string]TokenInfo{"good": {Owner: "alice", MaxSandboxes: 5}})

	r := gin.New()
	r.Use(ts.Auth())
	r.GET("/x", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"owner": tokenInfo(c).Owner})
	})

	cases := []struct {
		auth   string
		status int
	}{
		{"Bearer good", http.StatusOK},
		{"Bearer bad", http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("auth %q -> %d, want %d (body %s)", tc.auth, w.Code, tc.status, w.Body)
		}
		if tc.status == http.StatusOK && w.Body.String() != `{"owner":"alice"}` {
			t.Errorf("owner not propagated: %s", w.Body)
		}
	}
}
