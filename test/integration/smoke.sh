#!/usr/bin/env bash
#
# smoke.sh -- EmberVM M0 CI gate: the full happy path, once.
#
# env-check -> fetch assets -> build binaries -> build rootfs -> network ->
# plain boot -> jailer boot -> snapshot -> restore (file / uffd-lazy /
# uffd-prefetch), asserting probe seq continuity at every stage.
#
# Linux-only (Firecracker + KVM); needs root (self-elevates via sudo -E).
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "[smoke.sh] Linux-only (needs KVM + Firecracker); cannot run on $(uname -s)" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
WORK_DIR="${WORK_DIR:-$ROOT/work}"
RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
export ASSETS_DIR WORK_DIR RESULTS_DIR BIN_DIR FC_VERSION ARCH

log() { echo "[$(basename "$0")] $*" >&2; }

STEP_N=0
STEP_DESC="(startup)"
step() {
  STEP_N=$((STEP_N + 1))
  STEP_DESC="$1"
  log "=== STEP $STEP_N: $STEP_DESC"
}

cleanup() {
  rc=$?
  bash "$ROOT/scripts/teardown-network.sh" --id 0 >/dev/null 2>&1 || true
  # Kill leftover VMM / handler processes. Scoped to the versioned binary
  # names so plain and jailer-launched (chrooted argv) firecracker both match.
  pkill -f "firecracker-$FC_VERSION-$ARCH" 2>/dev/null || true
  pkill -f "$BIN_DIR/uffd-handler" 2>/dev/null || true
  if [ "$rc" -ne 0 ]; then
    log "FAILED at step $STEP_N ($STEP_DESC), exit code $rc"
  fi
  exit "$rc"
}
trap cleanup EXIT

mkdir -p "$WORK_DIR" "$RESULTS_DIR"

step "environment check (strict)"
bash "$ROOT/scripts/env-check.sh" --strict

step "fetch assets (fc=$FC_VERSION arch=$ARCH)"
bash "$ROOT/scripts/fetch-assets.sh"

step "ensure host tool binaries"
[ -x "$BIN_DIR/probe-server" ] || make -C "$ROOT" build

step "build guest rootfs"
bash "$ROOT/scripts/build-rootfs.sh"

step "setup network (id=0)"
bash "$ROOT/scripts/setup-network.sh" --id 0

step "plain boot + wait for probe"
J="$(bash "$ROOT/scripts/fc-boot.sh" --id 0 --wait-probe)"
log "plain boot probe: $J"
jq -e '.seq == 1' <<<"$J" >/dev/null \
  || { log "expected seq=1 on first probe after plain boot, got: $J"; exit 1; }

step "jailer boot + wait for probe"
J="$(bash "$ROOT/scripts/fc-boot.sh" --id 0 --wait-probe --jailer)"
log "jailer boot probe: $J"
jq -e '.seq == 1' <<<"$J" >/dev/null \
  || { log "expected seq=1 on first probe after jailer boot, got: $J"; exit 1; }

step "snapshot (mem=1024MiB dirty=128MiB)"
bash "$ROOT/scripts/fc-snapshot.sh" --id 0 --mem-mb 1024 --dirty-mb 128 --out "$WORK_DIR/snap0"

for mode in file uffd-lazy uffd-prefetch; do
  step "restore mode=$mode"
  R="$(bash "$ROOT/scripts/fc-restore.sh" --id 0 --snap-dir "$WORK_DIR/snap0" --mode "$mode" --iter 0)"
  log "restore result: $R"
  jq -e '.seq_ok == true' <<<"$R" >/dev/null \
    || { log "restore mode=$mode: seq_ok is not true: $R"; exit 1; }
  if [ "$mode" != "file" ]; then
    jq -e '(.faults_served + .bytes_copied_prefetch) > 0' <<<"$R" >/dev/null \
      || { log "restore mode=$mode: uffd handler did no work (faults_served + bytes_copied_prefetch == 0): $R"; exit 1; }
  fi
  log "mode=$mode t_api_ms=$(jq -r '.t_api_ms' <<<"$R") t_interactive_ms=$(jq -r '.t_interactive_ms' <<<"$R")"
done

log "SMOKE OK (fc=$FC_VERSION)"
