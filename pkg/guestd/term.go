package guestd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/embervm/embervm/pkg/guestapi"
)

// Terminal session policy. The idle window counts CLIENT data frames only —
// a busy `top` with nobody typing still times out; the ping loop is what
// keeps live-but-quiet peers distinguishable from dead ones. After a
// snapshot restore the restored guest holds shells whose WS peer no longer
// exists; the pong deadline bounds those orphans to ~termPongWait before
// their process group is killed.
const (
	termMaxSessions = 8
	termIdleTimeout = 30 * time.Minute
	termPingEvery   = 30 * time.Second
	termPongWait    = 60 * time.Second
)

// termProc is one shell under a PTY. Read/Write move raw bytes through the
// master side; implementations are per-OS (pty_linux.go / pty_stub.go).
type termProc interface {
	io.ReadWriter
	resize(cols, rows int)
	wait() int // reaps the shell (idempotent), returns its exit code
	kill()     // SIGKILLs the process group, reaps, closes the master
}

// handleTerm serves GET /term: a WebSocket PTY speaking
// guestapi.TermSubprotocol (binary = raw bytes, text = JSON control).
func (s *server) handleTerm(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{guestapi.TermSubprotocol},
	})
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	defer conn.CloseNow()

	// The limit is enforced after the upgrade so the browser sees the
	// documented close code instead of a generic handshake failure.
	if n := s.termSessions.Add(1); n > termMaxSessions {
		s.termSessions.Add(-1)
		conn.Close(websocket.StatusCode(guestapi.TermCloseSessionLimit), "session limit reached")
		return
	}
	defer s.termSessions.Add(-1)

	pt, err := startShell(termGeometry(r))
	if err != nil {
		conn.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer pt.kill()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PTY → WS. The master read failing (EIO once the shell and its slave
	// fds are gone) is the shell-exit signal: report the code, close 1000.
	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, rerr := pt.Read(buf)
			if n > 0 {
				if conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if rerr != nil {
				code := pt.wait()
				if payload, err := json.Marshal(guestapi.TermControl{Type: "exit", Code: code}); err == nil {
					_ = conn.Write(ctx, websocket.MessageText, payload)
				}
				conn.Close(websocket.StatusNormalClosure, "shell exited")
				return
			}
		}
	}()

	// Liveness. Pongs are answered inside conn.Read below; a peer that
	// cannot answer within termPongWait is gone.
	go func() {
		t := time.NewTicker(termPingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, termPongWait)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// WS → PTY (also the pong pump). Read deadline = the idle window.
	for {
		rctx, rcancel := context.WithTimeout(ctx, termIdleTimeout)
		typ, data, err := conn.Read(rctx)
		rcancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				conn.Close(websocket.StatusCode(guestapi.TermCloseIdle), "idle timeout")
			}
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := pt.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var ctl guestapi.TermControl
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" {
				pt.resize(ctl.Cols, ctl.Rows)
			}
		}
	}
}

// termGeometry parses ?cols=&rows= with sane defaults and bounds.
func termGeometry(r *http.Request) (cols, rows int) {
	cols, rows = 80, 24
	if v, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && v > 0 && v <= 1000 {
		cols = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && v > 0 && v <= 1000 {
		rows = v
	}
	return cols, rows
}
