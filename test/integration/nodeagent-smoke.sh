#!/usr/bin/env bash
#
# nodeagent-smoke.sh -- EmberVM M1 CI gate for the node agent.
#
# Fetches assets, builds binaries, then runs the Go KVM lifecycle test
# (pkg/nodeagent.TestAgentLifecycleKVM): build a template from a Docker image,
# create a microVM sandbox, exec + file R/W through guestd, pause, resume, and
# assert the guest process survived the restore (health seq 1 -> 2).
#
# Linux-only (Firecracker + KVM); needs root (self-elevates via sudo -E).
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "[nodeagent-smoke.sh] Linux-only (needs KVM + Firecracker); cannot run on $(uname -s)" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
export ASSETS_DIR BIN_DIR FC_VERSION ARCH

log() { echo "[$(basename "$0")] $*" >&2; }

cleanup() {
  rc=$?
  for id in 0 1; do bash "$ROOT/scripts/teardown-network.sh" --id "$id" >/dev/null 2>&1 || true; done
  pkill -f "firecracker-$FC_VERSION-$ARCH" 2>/dev/null || true
  pkill -f "$BIN_DIR/uffd-handler" 2>/dev/null || true
  [ "$rc" -ne 0 ] && log "FAILED (exit $rc)"
  exit "$rc"
}
trap cleanup EXIT

log "fetch assets (fc=$FC_VERSION arch=$ARCH)"
bash "$ROOT/scripts/fetch-assets.sh"

log "build host binaries"
make -C "$ROOT" build

# mkfs.ext4 is needed by the template builder (pkg/template).
command -v mkfs.ext4 >/dev/null 2>&1 || { log "installing e2fsprogs"; apt-get install -y e2fsprogs >&2; }

export EMBERVM_KVM_TESTS=1
export EMBERVM_KERNEL="$ASSETS_DIR/vmlinux-6.1.155"
export EMBERVM_FC_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH"
export EMBERVM_UFFD_BIN="$BIN_DIR/uffd-handler"
export EMBERVM_GUESTD_BIN="$BIN_DIR/guestd"
export EMBERVM_SCRIPT_DIR="$ROOT/scripts"
export EMBERVM_JAILER_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/jailer-$FC_VERSION-$ARCH"

log "running node agent lifecycle tests (M1 raw + M2 chunked pipeline)"
go test "$ROOT/pkg/nodeagent/" -run 'TestAgentLifecycleKVM|TestChunkedLifecycleKVM|TestJailedLifecycleKVM' -v -count=1 -timeout 20m \
  | tee /tmp/nodeagent-smoke.log
# A skipped lifecycle test must never masquerade as a pass (M1 CI lesson).
grep -q -- '--- PASS: TestAgentLifecycleKVM' /tmp/nodeagent-smoke.log
grep -q -- '--- PASS: TestChunkedLifecycleKVM' /tmp/nodeagent-smoke.log
grep -q -- '--- PASS: TestJailedLifecycleKVM' /tmp/nodeagent-smoke.log
