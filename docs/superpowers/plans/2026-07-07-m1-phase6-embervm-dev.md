# M1 Phase 6: `embervm dev` + deploy/singlenode Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use `- [ ]` checkboxes.

**Goal:** One command — `embervm dev` — runs the whole stack (migrations + API server + in-proc
node agent) in a single root process on one cloud VM, and `deploy/singlenode/install.sh`
provisions that VM from stock Ubuntu 24.04 (exit criterion 3).

**Architecture:** `cmd/embervm` is a small CLI with a `dev` subcommand that constructs the
concrete `nodeagent.Agent` in-process (no unix socket) and hands it to
`controlplane.NewServer`, so the API server calls the agent directly. This is the "single-mode
as a first-class citizen" form from docs/zh/05 §6.

## Global Constraints
- In-proc: `embervm dev` uses `nodeagent.New(...)` directly (not `nodeapi.NewClient`), so there
  is no socket hop. Same `controlplane.Server`, same REST surface.
- Requires root (Firecracker, netns, cgroups) and a reachable PostgreSQL.
- Storage backend selectable: `--zfs-pool` (default) or `--plain-root` for a no-ZFS quick start.

## Tasks
### Task 1: cmd/embervm CLI skeleton + dev subcommand
Files: `cmd/embervm/main.go`. Subcommands via a simple switch on `os.Args[1]` (no cobra dep):
`dev` and `version`. `dev` flags: `--database-url`, `--listen`, `--tokens-file`, `--zfs-pool`,
`--plain-root`, `--netns-pool`, `--script-dir`, `--work-dir`, `--kernel`, `--fc-bin`,
`--uffd-handler`, `--guestd-bin`, `--restore-mode`. Builds storage backend, netns pool (Setup),
in-proc `nodeagent.New`, store (+Migrate), tokens, `controlplane.NewServer`, then
`http.ListenAndServe`. On SIGINT/SIGTERM: pool.Teardown.

### Task 2: Makefile — build embervm
Add `cmd/embervm` to the `build` target's binary list (`bin/embervm`).

### Task 3: deploy/singlenode/install.sh
Replace the placeholder README's pointer with a real installer: on Ubuntu 24.04, apt-install
postgresql + zfsutils-linux + e2fsprogs, create the `embervm` role/db, create a ZFS pool (from
a `--pool-device` or a loopback file for trials), fetch FC assets (scripts/fetch-assets.sh),
`make build`, drop a systemd unit for `embervm dev`, and print next steps. Idempotent and
shellcheck-clean. Keep the actual `zpool create` behind an explicit flag so it never eats a
disk by surprise (default: loopback file with a printed warning).

### Task 4: deploy/singlenode/README.md
Document the one-VM quick start: prerequisites, `install.sh` usage, the dev token, and a
curl-based smoke (create template → create sandbox → exec).

### Task 5: CI — dev-mode smoke (KVM + PG)
Add an `embervm-dev-smoke` job to integration-kvm.yml: postgres service + KVM setup, build,
start `embervm dev --plain-root ... --database-url ...` in the background, then curl the REST
API: create template (alpine) → create sandbox → exec echo → pause → resume → kill. Asserts
HTTP 2xx and the exec output. Non-blocking-friendly teardown.

### Task 6: gate + commit + push + mark #16
`make lint && make test && GOOS=linux go build ./...`; push; watch integration-kvm
(embervm-dev-smoke) + lint-unit + controlplane-pg green.

## Verification
- `bin/embervm dev --help` lists the flags; `embervm version` prints the version.
- install.sh is shellcheck-clean and idempotent (dry-run on the runner in CI, or executed in
  the dev-smoke job).
- CI dev-smoke drives the full public REST lifecycle end-to-end on one process with a real
  microVM behind it.
