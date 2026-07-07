# M1 Phase 5: API Server v0 + PostgreSQL Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use `- [ ]` checkboxes.

**Goal:** A REST control plane (Gin + PostgreSQL) that persists templates and sandboxes,
enforces bearer-token auth + per-token quota, and drives a `nodeapi.Agent` for the full
lifecycle — the public face of EmberVM.

**Architecture:** `pkg/controlplane` holds the pgx store (embedded SQL migrations), the Gin
router/handlers, and auth. The server owns a `nodeapi.Agent` (in-proc for `embervm dev`, a
`nodeapi.Client` for split mode) and a `template.Build`-capable agent. PostgreSQL is the single
source of truth (docs/zh/04 §6); Redis is NOT used in M1 (Gateway routing is M4). Handlers are
unit-tested against a mock Agent + a real Postgres (or pgxmock); CI runs them against a
`postgres` service container.

**Tech Stack:** Go 1.24, gin-gonic/gin, jackc/pgx/v5 (+ stdlib database/sql for migrations via
embed.FS), no ORM.

## Global Constraints
- Endpoints per master-spec D1 (templates + sandboxes CRUD + pause/resume/snapshot/kill +
  guest exec/files proxy). Auth: `Authorization: Bearer <token>`; per-token `max_sandboxes`.
- v0.x API may break but each break is a CHANGELOG.md entry (docs/zh/05 §4).
- Schema per master-spec D6 (templates / sandboxes / sandbox_events). PostgreSQL is the sole
  authority; no homegrown distributed state.
- State transitions go through pkg/lifecycle; every transition writes a sandbox_events row.

## Design
**Schema** (embedded migration `0001_init.sql`): tables from master-spec D6 verbatim.
**Config**: `--database-url`, a tokens file (`token → {owner, max_sandboxes}`), listen addr.
**Handlers** map 1:1 to D1; template create kicks BuildTemplate then EnsureTemplate
(synchronous in M1 v0 — state BUILDING→READY/ERROR recorded); sandbox create validates quota
(count of non-terminal sandboxes for the owner < max_sandboxes) then Agent.CreateSandbox.
Guest exec/files proxy straight to Agent.Exec/ReadFile/WriteFile.

## Tasks
### Task 1: pgx store + migrations
Files: `pkg/controlplane/store.go`, `pkg/controlplane/migrations/0001_init.sql` (embed.FS),
`pkg/controlplane/store_test.go` (gated `EMBERVM_PG_TESTS=1`, uses `$EMBERVM_TEST_DATABASE_URL`).
Store methods: Migrate; CreateTemplate/GetTemplate/ListTemplates/SetTemplateState/DeleteTemplate;
CreateSandbox/GetSandbox/ListSandboxes/SetSandboxState(+event)/DeleteSandbox;
CountActiveSandboxes(owner). Types Template, Sandbox mirror the schema. Test: full CRUD +
event-append + active-count against real PG (skips if unset).

### Task 2: auth + quota middleware
Files: `pkg/controlplane/auth.go`, `pkg/controlplane/auth_test.go`. `TokenStore` from a map
(loaded from a JSON/env config); Gin middleware sets owner in context or 401. Quota check helper
used by the create handler. Pure unit tests.

### Task 3: Gin handlers + router
Files: `pkg/controlplane/server.go` (router + handlers), `pkg/controlplane/server_test.go`
(httptest + mock Agent + in-memory or real store). Every D1 endpoint; JSON errors
`{"error":...}`; correct status codes. Wire template create → Agent.BuildTemplate; sandbox
create → quota → Agent.CreateSandbox → persist RUNNING; pause/resume/snapshot/kill → Agent +
SetSandboxState; exec/files → Agent proxy.

### Task 4: cmd/apiserver
Files: `cmd/apiserver/main.go` (replace placeholder). Flags: `--database-url`, `--listen`,
`--tokens-file`, `--nodeagent-socket` (split) OR in-proc wiring is done by embervm dev (Phase 6).
For standalone apiserver, connect a `nodeapi.NewClient(socket)` Agent. Migrate on startup.

### Task 5: CI — postgres service
Modify `.github/workflows/lint-unit.yml`: add a `controlplane-pg` job with a `postgres:16`
service, `EMBERVM_PG_TESTS=1 EMBERVM_TEST_DATABASE_URL=postgres://... go test ./pkg/controlplane/`.

### Task 6: gate + commit + push + mark #15
`make lint && make test && GOOS=linux go build ./...`; push; watch lint-unit (incl.
controlplane-pg) + integration-kvm green.

## Verification
- Store CRUD + events + quota-count green against real PG in CI.
- Handler round-trips (mock Agent) green off-DB where possible.
- Full REST lifecycle exercised end-to-end in Phase 7's e2e job (real Agent + real PG).
