// Package guestapi defines the wire types spoken between guestd (the
// in-guest daemon, cmd/guestd) and the host side (API server proxy, e2e
// tests). guestd is the producer of these schemas; keep both ends in this
// one package so they cannot drift.
package guestapi

import "time"

// Port is the TCP port guestd listens on inside every template-built guest.
// The guest address is always 172.16.0.2 (per-sandbox netns, see docs/zh/02 §4).
const Port = 7777

// DefaultExecTimeoutS applies when ExecRequest.TimeoutS is zero.
const DefaultExecTimeoutS = 30

// MaxExecTimeoutS caps ExecRequest.TimeoutS.
const MaxExecTimeoutS = 300

// HealthResponse reports liveness plus a per-process monotone sequence
// counter: after a pause/snapshot/resume cycle the next probe must observe
// the pre-pause value +1, proving the SAME process survived (identical
// semantics to test/probe/server).
type HealthResponse struct {
	OK      bool   `json:"ok"`
	Seq     uint64 `json:"seq"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	// Resumes counts POST /resumed notifications this process has seen —
	// how many times the sandbox came back from a snapshot restore.
	Resumes uint64 `json:"resumes"`
	// Resource pressure (M6 autoscale signals; zero on guestd builds or
	// kernels that cannot report them). MemTotal moves with virtio-mem
	// resize; PSI values are /proc/pressure "some avg10" percentages.
	MemTotalKiB     uint64  `json:"mem_total_kib,omitempty"`
	MemAvailableKiB uint64  `json:"mem_available_kib,omitempty"`
	PSIMemSome10    float64 `json:"psi_mem_some10,omitempty"`
	PSICPUSome10    float64 `json:"psi_cpu_some10,omitempty"`
}

// ResumedResponse acknowledges a resume notification.
type ResumedResponse struct {
	Resumes uint64 `json:"resumes"`
	HookRan bool   `json:"hook_ran"`
}

// ExecRequest runs a command inside the guest.
type ExecRequest struct {
	Cmd      string   `json:"cmd"` // required; resolved via guest PATH
	Args     []string `json:"args,omitempty"`
	Env      []string `json:"env,omitempty"` // KEY=VALUE, appended to the guest environment
	Cwd      string   `json:"cwd,omitempty"`
	Stdin    []byte   `json:"stdin,omitempty"`     // base64 on the wire via encoding/json
	TimeoutS int      `json:"timeout_s,omitempty"` // 0 → DefaultExecTimeoutS; capped at MaxExecTimeoutS
}

// ExecResponse is the buffered result of an exec. On timeout the whole
// process group is killed and ExitCode is -1.
type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"` // stdout or stderr hit the per-stream cap
	DurationMs int64  `json:"duration_ms"`
}

// ErrorResponse is the body of every non-2xx guestd reply.
type ErrorResponse struct {
	Error string `json:"error"`
}

// DirEntry describes one entry of a listed guest directory (console file
// browser). Mode is fs.FileMode.String() (e.g. "drwxr-xr-x"); Symlink is the
// readlink target when the entry is a symlink (IsDir then reflects the
// target, so a browser can descend through directory links).
type DirEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mtime"`
	IsDir   bool      `json:"is_dir"`
	Symlink string    `json:"symlink,omitempty"`
}

// ListDirResponse answers GET /files?op=list. Entries are sorted
// directories-first, then by name; Truncated is set when the directory
// exceeded the guest-side entry cap.
type ListDirResponse struct {
	Path      string     `json:"path"`
	Entries   []DirEntry `json:"entries"`
	Truncated bool       `json:"truncated,omitempty"`
}

// Interactive terminal (GET /term, WebSocket).
//
// The socket speaks subprotocol TermSubprotocol: binary frames carry raw PTY
// bytes in both directions; text frames carry JSON TermControl messages.
// Initial geometry rides the query string (?cols=&rows=, defaults 80×24) so
// the first paint is right before the first resize message.
const TermSubprotocol = "embervm-term.v1"

// Close codes on a /term socket beyond the standard ones (1000 = shell
// exited, 1011 = internal error).
const (
	TermCloseSessionLimit = 4000 // too many concurrent sessions
	TermCloseIdle         = 4001 // no client frames for the idle window
)

// TermControl is a text-frame control message on a /term socket.
// client→guestd: {"type":"resize","cols":N,"rows":N}.
// guestd→client: {"type":"exit","code":N}, sent just before close 1000.
type TermControl struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code int    `json:"code,omitempty"`
}
