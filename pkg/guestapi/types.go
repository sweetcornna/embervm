// Package guestapi defines the wire types spoken between guestd (the
// in-guest daemon, cmd/guestd) and the host side (API server proxy, e2e
// tests). guestd is the producer of these schemas; keep both ends in this
// one package so they cannot drift.
package guestapi

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
