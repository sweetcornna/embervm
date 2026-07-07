package controlplane

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestResolveTokensFailsClosed(t *testing.T) {
	// No file and no insecure opt-in => error, never a silently-open server.
	if _, _, err := ResolveTokens("", false); err == nil {
		t.Error("ResolveTokens('', false) = nil error, want fail-closed error")
	}

	// Insecure opt-in => the dev token store, flagged as insecure.
	store, insecure, err := ResolveTokens("", true)
	if err != nil || store == nil || !insecure {
		t.Errorf("ResolveTokens('', true) = (%v, %v, %v), want (store, true, nil)", store, insecure, err)
	}
	if _, ok := store.Lookup(DevTokenName); !ok {
		t.Error("insecure store missing the dev token")
	}

	// A real tokens file => that store, not flagged insecure.
	f := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(f, []byte(`{"secret-tok":{"owner":"alice","max_sandboxes":5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, insecure, err = ResolveTokens(f, false)
	if err != nil || insecure {
		t.Fatalf("ResolveTokens(file, false) = (insecure=%v, err=%v)", insecure, err)
	}
	if info, ok := store.Lookup("secret-tok"); !ok || info.Owner != "alice" {
		t.Errorf("file token not loaded: %+v ok=%v", info, ok)
	}

	// An empty tokens file is an error, not an open server.
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveTokens(empty, false); err == nil {
		t.Error("ResolveTokens(empty-file, false) = nil error, want error")
	}
}

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
