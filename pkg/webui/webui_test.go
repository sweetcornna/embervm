package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSPAFallback: real assets serve as themselves; client-routed paths and
// / both serve the app shell, uncached.
func TestSPAFallback(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/sandboxes/123", "/deep/client/route"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}
		if !strings.Contains(strings.ToLower(w.Body.String()), "<title>") {
			t.Fatalf("GET %s did not serve the app shell", path)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("GET %s Cache-Control = %q, want no-cache (the shell must not stick)", path, cc)
		}
	}
}
