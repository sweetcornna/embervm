package guestd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
)

// limitedBuf keeps at most limit bytes and flips the shared truncated flag
// when more arrives. Write never fails so the exec pipes keep draining.
// Each stream has its own limitedBuf with a single writer goroutine
// (os/exec's copier), so no mutex is needed.
type limitedBuf struct {
	buf       bytes.Buffer
	limit     int64
	truncated *atomic.Bool
}

func (b *limitedBuf) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - int64(b.buf.Len())
	if room <= 0 {
		if n > 0 {
			b.truncated.Store(true)
		}
		return n, nil
	}
	if int64(n) > room {
		p = p[:room]
		b.truncated.Store(true)
	}
	b.buf.Write(p)
	return n, nil
}

func (s *server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req guestapi.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Cmd == "" {
		writeError(w, http.StatusBadRequest, errors.New("cmd is required"))
		return
	}
	timeoutS := req.TimeoutS
	switch {
	case timeoutS <= 0:
		timeoutS = guestapi.DefaultExecTimeoutS
	case timeoutS > guestapi.MaxExecTimeoutS:
		timeoutS = guestapi.MaxExecTimeoutS
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutS)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Cmd, req.Args...)
	setPgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	var truncated atomic.Bool
	stdout := &limitedBuf{limit: s.maxOutput, truncated: &truncated}
	stderr := &limitedBuf{limit: s.maxOutput, truncated: &truncated}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}
	cmd.Dir = req.Cwd

	start := time.Now()
	runErr := cmd.Run()

	resp := guestapi.ExecResponse{
		Stdout:     stdout.buf.Bytes(),
		Stderr:     stderr.buf.Bytes(),
		Truncated:  truncated.Load(),
		DurationMs: time.Since(start).Milliseconds(),
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// The command never ran: not found, bad cwd, etc. Distinguish
			// "could not start" (HTTP error) from "ran and failed" (200).
			writeError(w, http.StatusBadRequest, runErr)
			return
		}
		resp.ExitCode = exitErr.ExitCode() // -1 when killed by a signal
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		resp.TimedOut = true
		resp.ExitCode = -1
	}
	writeJSON(w, http.StatusOK, resp)
}
