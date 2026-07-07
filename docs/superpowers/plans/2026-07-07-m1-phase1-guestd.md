# M1 Phase 1: guestd v0 + pkg/guestapi Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The in-guest daemon (exec / file R/W / health) plus the shared wire types and the
host-side client the API server proxy will use, fully unit-tested without KVM.

**Architecture:** `pkg/guestapi` holds wire types + `Client` (host side). `pkg/guestd` holds
the portable HTTP handler so tests and the client exercise the real implementation.
`cmd/guestd` is a thin main that adds the linux-only PID 1 duties: mount pseudo-filesystems,
fork itself, reap orphans, restart the server child. Bench rootfs (probe-server) is untouched;
guestd ships only in template-built rootfs (Phase 2).

**Tech Stack:** Go 1.24 stdlib only (net/http, os/exec, golang.org/x/sys for wait4/mount).

## Global Constraints

- Module `github.com/embervm/embervm`; Go 1.24; `CGO_ENABLED=0 GOOS=linux` must build everything.
- Local verification only: `make lint && make test && GOOS=linux go build ./...` — CI is the real gate.
- guestd listens on `0.0.0.0:7777` (`guestapi.Port`); guest IP is always 172.16.0.2 (per-sandbox netns).
- `/healthz` seq = per-process monotone counter; after restore the next probe must observe prev+1
  (same restore-continuity semantics as test/probe/server).
- Exec: default timeout 30s, max 300s; stdout/stderr each capped (default 2MiB) with `truncated:true`;
  timeout kills the whole process group.

---

### Task 1: pkg/guestapi wire types

**Files:**
- Create: `pkg/guestapi/types.go`
- Test: none (pure data; exercised by every later test)

**Interfaces (Produces):**

```go
package guestapi

const Port = 7777
const DefaultExecTimeoutS = 30
const MaxExecTimeoutS = 300

type HealthResponse struct {
	OK      bool   `json:"ok"`
	Seq     uint64 `json:"seq"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

type ExecRequest struct {
	Cmd      string   `json:"cmd"`                 // required; resolved via guest PATH
	Args     []string `json:"args,omitempty"`
	Env      []string `json:"env,omitempty"`       // KEY=VALUE, appended to guest env
	Cwd      string   `json:"cwd,omitempty"`
	Stdin    []byte   `json:"stdin,omitempty"`     // base64 on the wire (encoding/json)
	TimeoutS int      `json:"timeout_s,omitempty"` // 0 → DefaultExecTimeoutS; capped at MaxExecTimeoutS
}

type ExecResponse struct {
	ExitCode   int    `json:"exit_code"`           // -1 when killed by signal/timeout
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"` // stdout or stderr hit the cap
	DurationMs int64  `json:"duration_ms"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
```

- [ ] Step 1: write types.go exactly as above; `git commit -m "feat(guestapi): wire types for guestd v0"` (commit folded into Task 3's commit is acceptable).

### Task 2: pkg/guestd portable HTTP handler

**Files:**
- Create: `pkg/guestd/server.go` (routes, health seq, files)
- Create: `pkg/guestd/exec.go` (portable exec logic) + `pkg/guestd/exec_unix.go` (pgid kill)
- Test: `pkg/guestd/server_test.go`

**Interfaces:**
- Consumes: guestapi types (Task 1).
- Produces: `func NewServer(opts Options) http.Handler`; `type Options struct { Version string; MaxOutputBytes int64 }`
  (zero values → defaults). Routes: `GET /healthz`, `POST /exec`, `GET /files?path=`, `PUT /files?path=[&mode=0644]`.

**Behavior spec (each bullet = a test):**
1. `GET /healthz` twice → `{ok:true, seq:1, ...}` then `seq:2`; pid == os.Getpid(); version echoed.
2. `POST /exec {cmd:"sh", args:["-c","printf hi"]}` → 200, exit_code 0, stdout "hi".
3. `POST /exec {cmd:"sh", args:["-c","exit 3"]}` → 200, exit_code 3.
4. `POST /exec {cmd:"sh", args:["-c","cat"], stdin:"ping"}` → stdout "ping".
5. `POST /exec {cmd:"sh", args:["-c","pwd"], cwd:<tmpdir>}` → stdout == EvalSymlinks(tmpdir)+"\n".
6. `POST /exec {cmd:"sh", args:["-c","sleep 5"], timeout_s:1}` → timed_out true, exit_code -1, duration < 5000ms; the sleep's process group is dead.
7. With `MaxOutputBytes: 64`: `sh -c 'head -c 1000 /dev/zero'` → truncated true, len(stdout) == 64.
8. `POST /exec {}` (no cmd) → 400 `{"error":...}`.
9. `PUT /files?path=<tmp>/a/b/c.txt&mode=0600` body "data" → 204; parent dirs created; file mode 0600; `GET /files?path=...` → 200 "data".
10. `GET /files?path=<tmp>/missing` → 404; `GET /files?path=relative` → 400; GET on a directory → 400.

**Implementation notes (the tricky parts, full code):**

```go
// exec_unix.go  //go:build unix
func setPgid(cmd *exec.Cmd)      { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil { return nil }
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
```

```go
// exec.go core: context timeout + group kill + capped buffers
ctx, cancel := context.WithTimeout(r.Context(), timeout)
defer cancel()
cmd := exec.CommandContext(ctx, req.Cmd, req.Args...)
setPgid(cmd)
cmd.Cancel = func() error { return killGroup(cmd) }
cmd.WaitDelay = 2 * time.Second
var stdout, stderr limitedBuf // Write keeps first N bytes, sets shared truncated flag
cmd.Stdout, cmd.Stderr = &stdout, &stderr
if len(req.Stdin) > 0 { cmd.Stdin = bytes.NewReader(req.Stdin) }
cmd.Env = append(os.Environ(), req.Env...)
cmd.Dir = req.Cwd
err := cmd.Run()
// exit code: ExitError → code; ctx deadline → TimedOut=true, ExitCode=-1; other err → 500 pre-start / -1 post-start
```

- [ ] Step 1: write server_test.go covering behaviors 1-10 (table-driven where natural)
- [ ] Step 2: `go test ./pkg/guestd/` → FAIL (package missing)
- [ ] Step 3: implement server.go/exec.go/exec_unix.go
- [ ] Step 4: `go test ./pkg/guestd/` → PASS; `make lint` clean
- [ ] Step 5: commit `feat(guestd): portable exec/files/health handler`

### Task 3: pkg/guestapi host-side Client

**Files:**
- Create: `pkg/guestapi/client.go`
- Test: `pkg/guestapi/client_test.go` (against the REAL handler: `httptest.NewServer(guestd.NewServer(...))`)

**Interfaces (Produces — the apiserver proxy in Phase 5 consumes exactly these):**

```go
func NewClient(baseURL string, hc *http.Client) *Client // hc nil → &http.Client{Timeout: 0} (per-call ctx)
func (c *Client) Health(ctx context.Context) (*HealthResponse, error)
func (c *Client) Exec(ctx context.Context, req *ExecRequest) (*ExecResponse, error)
func (c *Client) ReadFile(ctx context.Context, path string) ([]byte, error)
func (c *Client) WriteFile(ctx context.Context, path string, mode fs.FileMode, data []byte) error
```
Non-2xx → error wrapping ErrorResponse.Error. `hc` injection point is how Phase 4 dials into a netns.

- [ ] Step 1: write client_test.go (health roundtrip incl. seq continuity, exec echo, file write+read, 404 → error containing server message)
- [ ] Step 2: `go test ./pkg/guestapi/` → FAIL
- [ ] Step 3: implement client.go
- [ ] Step 4: `go test ./pkg/guestapi/` → PASS
- [ ] Step 5: commit `feat(guestapi): host-side client`

### Task 4: cmd/guestd main — PID 1 init + serve

**Files:**
- Modify: `cmd/guestd/main.go` (replace placeholder)
- Create: `cmd/guestd/init_linux.go`, `cmd/guestd/init_stub.go` (`//go:build !linux`, returns error)
- Test: manual build only (`GOOS=linux go build`); real validation is Phase 2's boot-in-CI. Unit-testing PID 1 is not possible off-guest.

**Behavior:**
- Flags: `--addr :7777`. Version const `v0.1.0-m1` passed into guestd.Options.
- `os.Getpid() == 1` and env `EMBERVM_GUESTD_CHILD` unset → `runInit()`:
  mount proc→/proc, sysfs→/sys, devtmpfs→/dev, tmpfs→/tmp,/run (each best-effort: ignore
  EBUSY/ENOENT — kernel may have auto-mounted); then loop: fork/exec `/proc/self/exe` with
  `EMBERVM_GUESTD_CHILD=1` + original args; `unix.Wait4(-1, ...)` reaping everything; when the
  wait'd pid is the server child, log + 1s backoff + respawn (systemd Restart=always analogue).
- Otherwise: `http.ListenAndServe(addr, guestd.NewServer(...))` with stdout log line
  `guestd listening addr=... pid=...` (mirrors probe-server's startup line convention).

```go
// init_linux.go core
func mountAll() {
	for _, m := range []struct{ src, dst, typ string; flags uintptr }{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
		{"tmpfs", "/tmp", "tmpfs", 0},
		{"tmpfs", "/run", "tmpfs", 0},
	} {
		_ = os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.typ, m.flags, ""); err != nil && !errors.Is(err, unix.EBUSY) {
			fmt.Fprintf(os.Stderr, "guestd: mount %s: %v\n", m.dst, err)
		}
	}
}

func runInit() error {
	mountAll()
	for {
		child, err := spawnChild() // exec.Command("/proc/self/exe", os.Args[1:]...) + env
		if err != nil { return err }
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, 0, nil)
			if err == unix.EINTR { continue }
			if err != nil { return err }
			if pid == child { break } // respawn after backoff
		}
		fmt.Fprintln(os.Stderr, "guestd: server child exited; respawning in 1s")
		time.Sleep(time.Second)
	}
}
```
Note: the reaper only ever calls Wait4 in the PID 1 process, whose direct children are the
server child and reparented orphans — it can never race the server child's own os/exec waits.

- [ ] Step 1: implement all three files
- [ ] Step 2: `make lint && GOOS=linux go build ./... && go build ./...` (macOS build uses init_stub)
- [ ] Step 3: `make test` → all packages PASS
- [ ] Step 4: commit `feat(guestd): PID 1 init + HTTP daemon entrypoint`

### Task 5: push + CI gate

- [ ] Step 1: `git push` → watch `lint-unit` and `integration-kvm` (both must stay green; guestd is not yet in any rootfs so smoke behavior is unchanged)
- [ ] Step 2: mark task #11 complete; proceed to Phase 2 plan.

## Verification

- `make lint && make test` locally green; `GOOS=linux CGO_ENABLED=0 go build ./...` green.
- CI lint-unit + integration-kvm green on the pushed commit.
- End-to-end proof (boot guestd as PID 1 inside a real microVM) lands with Phase 2's
  template-boot CI job — tracked there, not here.
