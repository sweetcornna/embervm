package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dialerAgent is mockAgent plus the GuestDialer data path: DialGuest lands
// on a test HTTP server standing in for the guest.
type dialerAgent struct {
	mockAgent
	addr     string // guest listener host:port
	dialErr  error
	lastDial struct {
		id   string
		port int
	}
}

func (d *dialerAgent) DialGuest(_ context.Context, id string, port int) (net.Conn, error) {
	d.lastDial.id, d.lastDial.port = id, port
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return net.Dial("tcp", d.addr)
}

// TestGuestProxyTwoHops drives the full M4 gateway data path below the
// apiserver: Client.GuestProxy (apiserver-side hop) → node daemon over UDS →
// guestProxy → netns dial → guest. Method, subpath, query, and body must
// arrive intact; status and body must come back.
func TestGuestProxyTwoHops(t *testing.T) {
	var got struct{ method, path, query, body string }
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.query, got.body = r.Method, r.URL.Path, r.URL.RawQuery, string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "pong %s", r.URL.Path)
	}))
	defer guest.Close()

	d := &dialerAgent{addr: strings.TrimPrefix(guest.URL, "http://")}
	c := serveMock(t, d)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/echo?x=1", strings.NewReader("ping"))
	c.GuestProxy("sb1", 8080).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "pong /api/echo" {
		t.Errorf("body = %q", rec.Body)
	}
	if got.method != http.MethodPost || got.path != "/api/echo" || got.query != "x=1" || got.body != "ping" {
		t.Errorf("guest saw %+v", got)
	}
	if d.lastDial.id != "sb1" || d.lastDial.port != 8080 {
		t.Errorf("dialed %+v", d.lastDial)
	}
}

func TestGuestProxyRootPath(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "root %s", r.URL.Path)
	}))
	defer guest.Close()

	d := &dialerAgent{addr: strings.TrimPrefix(guest.URL, "http://")}
	c := serveMock(t, d)

	rec := httptest.NewRecorder()
	c.GuestProxy("sb1", 8080).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "root /" {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body)
	}
}

func TestGuestProxyBadPort(t *testing.T) {
	d := &dialerAgent{}
	c := serveMock(t, d)

	rec := httptest.NewRecorder()
	c.GuestProxy("sb1", 99999).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestGuestProxyDialError(t *testing.T) {
	d := &dialerAgent{dialErr: errors.New("no such sandbox")}
	c := serveMock(t, d)

	rec := httptest.NewRecorder()
	c.GuestProxy("sb1", 8080).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no such sandbox") {
		t.Errorf("error body = %q", rec.Body)
	}
}

// TestGuestProxyNotRegisteredWithoutDialer pins the capability gate: a node
// whose agent cannot enter the netns must not expose the proxy route.
func TestGuestProxyNotRegisteredWithoutDialer(t *testing.T) {
	c := serveMock(t, &mockAgent{})

	rec := httptest.NewRecorder()
	c.GuestProxy("sb1", 8080).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route absent)", rec.Code)
	}
}
