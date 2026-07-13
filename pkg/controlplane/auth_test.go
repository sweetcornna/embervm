package controlplane

import (
	"encoding/base64"
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

func TestWSProtocolToken(t *testing.T) {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	mkReq := func(upgrade bool, protocols ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		if upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		for _, p := range protocols {
			r.Header.Add("Sec-WebSocket-Protocol", p)
		}
		return r
	}

	// Happy path: token extracted, credential entry stripped, the real
	// application subprotocol survives for upstream negotiation.
	r := mkReq(true, "bearer."+b64("secret-tok")+", embervm-term.v1")
	token, ok := wsProtocolToken(r)
	if !ok || token != "secret-tok" {
		t.Fatalf("wsProtocolToken = (%q,%v), want (secret-tok,true)", token, ok)
	}
	if got := r.Header.Get("Sec-WebSocket-Protocol"); got != "embervm-term.v1" {
		t.Errorf("header after strip = %q, want embervm-term.v1", got)
	}

	// The credential alone: the header must disappear entirely.
	r = mkReq(true, "bearer."+b64("secret-tok"))
	if _, ok := wsProtocolToken(r); !ok {
		t.Fatal("lone credential entry not extracted")
	}
	if vals := r.Header.Values("Sec-WebSocket-Protocol"); len(vals) != 0 {
		t.Errorf("header not removed: %v", vals)
	}

	// Separate header lines (both forms are legal HTTP).
	r = mkReq(true, "embervm-term.v1", "bearer."+b64("tok2"))
	if token, ok := wsProtocolToken(r); !ok || token != "tok2" {
		t.Errorf("multi-line header: (%q,%v)", token, ok)
	}

	// Rejections: no upgrade; no credential entry; invalid base64 (entry
	// then survives as an ordinary subprotocol).
	if _, ok := wsProtocolToken(mkReq(false, "bearer."+b64("x"))); ok {
		t.Error("non-upgrade request must not yield a token")
	}
	if _, ok := wsProtocolToken(mkReq(true, "graphql-ws")); ok {
		t.Error("no credential entry must not yield a token")
	}
	r = mkReq(true, "bearer.!!!not-base64!!!")
	if _, ok := wsProtocolToken(r); ok {
		t.Error("invalid base64 must not yield a token")
	}
	if got := r.Header.Get("Sec-WebSocket-Protocol"); got != "bearer.!!!not-base64!!!" {
		t.Errorf("undecodable entry must be preserved, got %q", got)
	}
}

func TestAuthMiddlewareWSSubprotocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ts := NewTokenStore(map[string]TokenInfo{"good": {Owner: "alice", MaxSandboxes: 5}})

	var seenProtocols string
	r := gin.New()
	r.Use(ts.Auth())
	r.GET("/x", func(c *gin.Context) {
		seenProtocols = c.GetHeader("Sec-WebSocket-Protocol")
		c.JSON(http.StatusOK, gin.H{"owner": tokenInfo(c).Owner})
	})

	ws := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Protocol",
			"bearer."+base64.RawURLEncoding.EncodeToString([]byte(token))+", embervm-term.v1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := ws("good"); w.Code != http.StatusOK || w.Body.String() != `{"owner":"alice"}` {
		t.Errorf("ws auth = %d %s", w.Code, w.Body)
	}
	// The handler (and so anything proxied) must never see the credential.
	if seenProtocols != "embervm-term.v1" {
		t.Errorf("handler saw protocols %q, credential leaked past Auth", seenProtocols)
	}
	if w := ws("bad"); w.Code != http.StatusUnauthorized {
		t.Errorf("ws bad token = %d, want 401", w.Code)
	}

	// A non-upgrade request cannot authenticate via the subprotocol header.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Sec-WebSocket-Protocol",
		"bearer."+base64.RawURLEncoding.EncodeToString([]byte("good")))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("plain request with subprotocol token = %d, want 401", w.Code)
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
