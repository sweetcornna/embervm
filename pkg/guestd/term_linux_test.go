//go:build linux

package guestd

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/embervm/embervm/pkg/guestapi"
)

// dialTerm opens one /term session against a test server.
func dialTerm(t *testing.T, ctx context.Context, srv *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/term" + query
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols: []string{guestapi.TermSubprotocol},
	})
	if err != nil {
		t.Fatalf("dial /term: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if got := conn.Subprotocol(); got != guestapi.TermSubprotocol {
		t.Fatalf("subprotocol = %q, want %q", got, guestapi.TermSubprotocol)
	}
	return conn
}

// readUntil pumps frames until the accumulated binary output contains want,
// collecting any control messages seen along the way.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, want string) (output string, controls []guestapi.TermControl) {
	t.Helper()
	var out strings.Builder
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read (have %q, want substring %q): %v", out.String(), want, err)
		}
		switch typ {
		case websocket.MessageBinary:
			out.Write(data)
			if strings.Contains(out.String(), want) {
				return out.String(), controls
			}
		case websocket.MessageText:
			var ctl guestapi.TermControl
			if json.Unmarshal(data, &ctl) == nil {
				controls = append(controls, ctl)
			}
		}
	}
}

func send(t *testing.T, ctx context.Context, conn *websocket.Conn, line string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageBinary, []byte(line)); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}

func TestTermEchoAndResize(t *testing.T) {
	srv := newTestServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := dialTerm(t, ctx, srv, "?cols=100&rows=30")

	// A marker built at runtime so the PTY's echo of the typed command line
	// cannot satisfy the assertion by itself.
	send(t, ctx, conn, "echo term-\"work\"s\n")
	readUntil(t, ctx, conn, "term-works")

	// stty must report the initial geometry, then the resized one.
	send(t, ctx, conn, "stty size\n")
	readUntil(t, ctx, conn, "30 100")

	ctl, _ := json.Marshal(guestapi.TermControl{Type: "resize", Cols: 50, Rows: 20})
	if err := conn.Write(ctx, websocket.MessageText, ctl); err != nil {
		t.Fatal(err)
	}
	send(t, ctx, conn, "stty size\n")
	readUntil(t, ctx, conn, "20 50")
}

func TestTermExitCode(t *testing.T) {
	srv := newTestServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := dialTerm(t, ctx, srv, "")

	send(t, ctx, conn, "exit 3\n")
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// The exit control frame must have arrived before the close.
			t.Fatalf("closed before exit control frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ctl guestapi.TermControl
		if json.Unmarshal(data, &ctl) == nil && ctl.Type == "exit" {
			if ctl.Code != 3 {
				t.Errorf("exit code = %d, want 3", ctl.Code)
			}
			return
		}
	}
}

func TestTermCloseKillsShell(t *testing.T) {
	srv := newTestServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn := dialTerm(t, ctx, srv, "")

	// The markers are quote-split in the typed command so the PTY's echo of
	// the input line can never contain them; only the shell's expansion does.
	send(t, ctx, conn, `echo pi""d:$$:x""yz`+"\n")
	out, _ := readUntil(t, ctx, conn, ":xyz")
	i := strings.LastIndex(out, "pid:")
	rest := out[i+len("pid:"):]
	pidStr := rest[:strings.Index(rest, ":")]
	pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
	if err != nil {
		t.Fatalf("parse pid from %q: %v", out, err)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return // reaped
		}
		// A zombie still has a /proc entry; state Z counts as dead enough
		// only once the parent reaps it, which kill() does synchronously —
		// so plain existence is the right check.
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("shell pid %d still alive after close", pid)
}

func TestTermSessionLimit(t *testing.T) {
	srv := newTestServer(t, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conns := make([]*websocket.Conn, 0, termMaxSessions)
	for i := range termMaxSessions {
		c := dialTerm(t, ctx, srv, "")
		// The session counter increments just after the 101; confirming the
		// shell answers serializes it so the 9th dial cannot race ahead.
		send(t, ctx, c, `echo u""p`+strconv.Itoa(i)+"\n")
		readUntil(t, ctx, c, "up"+strconv.Itoa(i))
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			_ = c.CloseNow()
		}
	}()

	over := dialTerm(t, ctx, srv, "")
	_, _, err := over.Read(ctx)
	var ce websocket.CloseError
	if !errorsAsClose(err, &ce) || int(ce.Code) != guestapi.TermCloseSessionLimit {
		t.Fatalf("9th session: err = %v, want close %d", err, guestapi.TermCloseSessionLimit)
	}
}

// errorsAsClose unwraps a websocket.CloseError (kept tiny to avoid importing
// errors just for one call).
func errorsAsClose(err error, ce *websocket.CloseError) bool {
	c := websocket.CloseStatus(err)
	if c == -1 {
		return false
	}
	ce.Code = c
	return true
}
