// Package guestd implements the EmberVM in-guest daemon's HTTP surface:
// process exec, file read/write, and a health endpoint whose per-process
// sequence counter lets restore tests assert that the SAME process survived
// a pause/snapshot/resume cycle (mirroring test/probe/server).
//
// The package is portable so unit tests and the host-side client exercise
// the real handler without a VM; the linux-only PID 1 duties live in
// cmd/guestd. Wire types are defined in pkg/guestapi.
package guestd

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"context"
	"fmt"
	"github.com/embervm/embervm/pkg/guestapi"
	"os/exec"
	"time"
)

// defaultMaxOutputBytes caps each exec output stream (stdout, stderr).
const defaultMaxOutputBytes = 2 << 20

// Options configures the guestd HTTP handler. Zero values pick defaults.
type Options struct {
	Version        string
	MaxOutputBytes int64 // per-stream exec output cap; 0 → 2MiB
}

type server struct {
	version   string
	maxOutput int64
	seq       atomic.Uint64
	resumes   atomic.Uint64
}

// NewServer returns the guestd HTTP handler.
func NewServer(opts Options) http.Handler {
	s := &server{version: opts.Version, maxOutput: opts.MaxOutputBytes}
	if s.maxOutput <= 0 {
		s.maxOutput = defaultMaxOutputBytes
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /resumed", s.handleResumed)
	mux.HandleFunc("POST /exec", s.handleExec)
	mux.HandleFunc("GET /files", s.handleReadFile)
	mux.HandleFunc("PUT /files", s.handleWriteFile)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, guestapi.ErrorResponse{Error: err.Error()})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	h := guestapi.HealthResponse{
		OK:      true,
		Seq:     s.seq.Add(1),
		PID:     os.Getpid(),
		Version: s.version,
		Resumes: s.resumes.Load(),
	}
	fillPressure(&h)
	writeJSON(w, http.StatusOK, h)
}

// fillPressure reports the guest's memory/CPU pressure for the host's
// autoscale engine (M6). Best-effort: a kernel without PSI, or a non-Linux
// test build, just reports zeros and the engine skips the sandbox.
func fillPressure(h *guestapi.HealthResponse) {
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				continue
			}
			switch f[0] {
			case "MemTotal:":
				h.MemTotalKiB = v
			case "MemAvailable:":
				h.MemAvailableKiB = v
			}
		}
	}
	h.PSIMemSome10 = psiSomeAvg10("/proc/pressure/memory")
	h.PSICPUSome10 = psiSomeAvg10("/proc/pressure/cpu")
}

// psiSomeAvg10 parses the "some ... avg10=X.XX" line of a PSI file.
func psiSomeAvg10(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0
				}
				return n
			}
		}
	}
	return 0
}

// resumeHookPath is executed (best-effort) on every resume notification:
// token refresh, cache invalidation — whatever the image needs after a
// pause/restore gap (docs/zh/03 §3 M2 correctness hooks).
const resumeHookPath = "/etc/embervm/resume-hook"

// handleResumed is called by the node agent after a snapshot restore. The
// kernel already re-armed kvm-clock and reseeded the RNG via VMGenID; this
// hook exists for userspace concerns.
func (s *server) handleResumed(w http.ResponseWriter, r *http.Request) {
	n := s.resumes.Add(1)
	// HookRan means the hook was found and STARTED — it runs async with a
	// 5s bound and only logs its own failures; "ran to completion
	// successfully" is not part of the wire contract.
	hookRan := false
	if st, err := os.Stat(resumeHookPath); err == nil && st.Mode()&0o111 != 0 {
		hookRan = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, resumeHookPath).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "guestd: resume-hook: %v: %s\n", err, out)
			}
		}()
	}
	writeJSON(w, http.StatusOK, guestapi.ResumedResponse{Resumes: n, HookRan: hookRan})
}

// absPathParam extracts and validates the required ?path= query parameter.
func absPathParam(r *http.Request) (string, error) {
	path := r.URL.Query().Get("path")
	if path == "" {
		return "", errors.New("missing path parameter")
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	return path, nil
}

func (s *server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	path, err := absPathParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
		return
	case fi.IsDir():
		writeError(w, http.StatusBadRequest, errors.New("path is a directory"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	// Exactly the advertised length: a file growing between Stat and here
	// would otherwise overrun Content-Length and corrupt the response
	// framing (a shrinking file still truncates — the client sees the
	// mismatch and errors, which is the honest outcome).
	_, _ = io.CopyN(w, f, fi.Size())
}

func (s *server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	path, err := absPathParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mode := fs.FileMode(0o644)
	if raw := r.URL.Query().Get("mode"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("mode must be octal, e.g. 0644"))
			return
		}
		mode = fs.FileMode(parsed)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		f.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := f.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// O_CREATE mode only applies to newly created files; chmod makes the
	// requested mode stick when overwriting an existing one.
	if err := os.Chmod(path, mode); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
