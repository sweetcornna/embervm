# M1 Phase 7: Exit-Criteria e2e + ADR + tag Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or
> superpowers:executing-plans. Steps use `- [ ]` checkboxes. This phase proves the three M1
> exit criteria and closes the milestone.

**Goal:** Demonstrate the M1 exit criteria in CI — 单机 20 并发; 热恢复 <1s（含 15GB 数据盘）; 单台云
VM 可完整部署 — record the M1 design decisions as ADR-0002, update the roadmap, and tag `m1`.

## Global Constraints
- Exit criteria measured CI-relative per ADR-0001 (nested virtualization; bare-metal
  re-measurement remains a tracked follow-up). The <1s target is the hot-resume path alone
  (uffd load → interactive), not pause+resume round trip.
- Reuse the concrete Agent + storage + netns already built; no new runtime code beyond a
  timing/concurrency test harness.

## Tasks
### Task 1: exit-criteria KVM test
Files: `pkg/nodeagent/agent_exit_test.go` (`//go:build linux`, env-gated `EMBERVM_KVM_TESTS=1`).
Two tests reusing the KVM harness (env asset paths):
- `TestConcurrency20`: build one template, create 20 sandboxes concurrently (256MiB, 1 vCpu,
  1 GiB data), assert all reach RUNNING and each guestd answers an exec; tear all down. Uses a
  netns pool of ≥24.
- `TestHotResumeUnder1s15GiB`: create one sandbox with a **15 GiB** data.raw, pause, then time
  ResumeSandbox until guestd is interactive; assert the resume (load→interactive) is <1s
  (log the number regardless). Uses ZFS backend on the loop pool so the 15 GiB clone/attach is
  O(1) (the criterion explicitly includes the data disk); falls back to plain if ZFS absent
  but then still asserts the sparse disk doesn't dominate.

Instrument ResumeSandbox to expose the load→interactive duration (return it in SandboxStatus or
via a test hook) so the assertion measures the right segment, not process spawn.

### Task 2: e2e-m1.yml workflow
Files: `.github/workflows/e2e-m1.yml`. Jobs (KVM + where needed a loop ZFS pool + postgres):
- `exit-criteria`: KVM + loop ZFS pool (ensure_zfs style) + assets, run the two exit tests on
  FC v1.16.1.
- `install`: run `deploy/singlenode/install.sh --no-systemd` far enough to prove it provisions
  (PG + pool + build) on a stock runner, or a dry-run that stops before starting the service.
Both on push to main + workflow_dispatch. Upload VM logs on failure.

### Task 3: ADR-0002
Files: `docs/adr/0002-m1-control-plane-shape.md`. Record decisions D1–D10: REST surface, the
nodeapi.Agent interface + in-proc/split wiring, guestd PID-1 model, template pipeline, ZFS
layout, PG schema, embervm dev, lifecycle machine, hot-resume path, and the jailer-deferred +
templates-node-global + Redis/gRPC-deferred rationale.

### Task 4: docs + CHANGELOG + memory
- README.md / README.zh-CN.md: check the M1 roadmap box (note CI-relative numbers, bare-metal
  re-measurement pending), add the `embervm dev` quick-start link.
- CHANGELOG.md: start it; add the `v0.1.0-m1` entry summarizing the milestone (API may break in
  v0.x per docs/zh/05 §4).
- Update the embervm-m0-status memory (or add embervm-m1-status) with M1 completion evidence.

### Task 5: gate + tag
`make lint && make test && GOOS=linux go build ./...`; push; watch e2e-m1 + all M0 suites green
3× (per the M0 exit pattern); then `git tag -a m1 -m "..."` and push the tag.

## Verification
- e2e-m1 green: 20 concurrent sandboxes all RUNNING+exec; hot resume <1s with 15 GiB data disk
  (number logged); install.sh provisions a node.
- All prior suites (lint-unit, controlplane-pg, storage-zfs, integration-kvm smoke +
  nodeagent-smoke + dev-smoke) green on the same commit.
- README roadmap M1 checked both languages; ADR-0002 + CHANGELOG committed; `m1` tag pushed.
