package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// TestCreateSandboxRejectsBadGeometry: negative or absurd resources must be
// refused at the door — a negative memory_mib fits every node ("freeMem <
// memoryMiB" is always false) and corrupts NodeUsage sums.
func TestCreateSandboxRejectsBadGeometry(t *testing.T) {
	h := newTestServer(t, &cpMockAgent{})
	w := callAs(h, "tok", http.MethodPost, "/v0/templates",
		map[string]string{"name": "geo", "image": "alpine"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create template: %d %s", w.Code, w.Body)
	}
	var tpl struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)

	for _, body := range []map[string]any{
		{"template_id": tpl.ID, "memory_mib": -1},
		{"template_id": tpl.ID, "vcpus": -2},
		{"template_id": tpl.ID, "data_disk_gib": -3},
		{"template_id": tpl.ID, "memory_mib": 1 << 30},
	} {
		w := callAs(h, "tok", http.MethodPost, "/v0/sandboxes", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("create with %v = %d, want 400", body, w.Code)
		}
	}
}

// TestCreateSandboxRequiresReadyTemplate: a BUILDING/ERROR template must be
// refused with a clean 409 instead of failing deep inside the node agent
// (where a partial create can orphan resources).
func TestCreateSandboxRequiresReadyTemplate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := testStore(t)
	tokens := NewTokenStore(map[string]TokenInfo{"tok": {Owner: "alice", MaxSandboxes: 2}})
	h := NewServer(store, &cpMockAgent{}, tokens, nil, nil).Handler()

	tpl, err := store.CreateTemplate(context.Background(), uuid.NewString(), "building", "alpine")
	if err != nil {
		t.Fatal(err)
	}
	w := callAs(h, "tok", http.MethodPost, "/v0/sandboxes",
		map[string]any{"template_id": tpl.ID})
	if w.Code != http.StatusConflict {
		t.Fatalf("create against %s template = %d, want 409: %s", tpl.State, w.Code, w.Body)
	}
}

// TestTokenStoreHashedLookup pins the digest-based lookup: the same token
// resolves, an unknown one does not (tokens are stored hashed so lookups
// never compare raw secret material).
func TestTokenStoreHashedLookup(t *testing.T) {
	ts := NewTokenStore(map[string]TokenInfo{"secret-token": {Owner: "alice", MaxSandboxes: 3}})
	info, ok := ts.Lookup("secret-token")
	if !ok || info.Owner != "alice" || info.MaxSandboxes != 3 {
		t.Fatalf("Lookup(known) = %+v, %v", info, ok)
	}
	if _, ok := ts.Lookup("secret-tokem"); ok {
		t.Fatal("Lookup(unknown) succeeded")
	}
	if _, ok := ts.Lookup(""); ok {
		t.Fatal("Lookup(empty) succeeded")
	}
}
