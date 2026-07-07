#!/usr/bin/env bash
#
# dev-smoke.sh -- EmberVM M1 end-to-end gate through `embervm dev`.
#
# Boots the single-process stack (API + in-proc node agent) against a real
# PostgreSQL and drives the public REST API through the full lifecycle:
# build a template from a Docker image, create a sandbox microVM, exec a
# command, pause, resume, and kill -- asserting HTTP status and exec output.
#
# Linux-only (Firecracker + KVM); needs root (self-elevates via sudo -E).
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "[dev-smoke.sh] Linux-only (needs KVM + Firecracker)" >&2
  exit 1
fi
if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
DB_URL="${EMBERVM_DATABASE_URL:-postgres://embervm:embervm@localhost:5432/embervm?sslmode=disable}"
API="localhost:8080"
AUTH="Authorization: Bearer dev-token"
export ASSETS_DIR BIN_DIR FC_VERSION ARCH

log() { echo "[dev-smoke.sh] $*" >&2; }
die() { log "FAILED: $*"; exit 1; }

EMBERVM_PID=""
WORK_DIR="$(mktemp -d)"
cleanup() {
  rc=$?
  if [ -n "$EMBERVM_PID" ]; then kill "$EMBERVM_PID" 2>/dev/null || true; fi
  for id in $(seq 0 5); do bash "$ROOT/scripts/teardown-network.sh" --id "$id" >/dev/null 2>&1 || true; done
  pkill -f "firecracker-$FC_VERSION-$ARCH" 2>/dev/null || true
  pkill -f "$BIN_DIR/uffd-handler" 2>/dev/null || true
  rm -rf "$WORK_DIR"
  [ "$rc" -ne 0 ] && log "exit $rc"
  exit "$rc"
}
trap cleanup EXIT

command -v jq >/dev/null 2>&1 || { apt-get install -y jq >&2; }
command -v mkfs.ext4 >/dev/null 2>&1 || { apt-get install -y e2fsprogs >&2; }

log "fetch assets + build"
bash "$ROOT/scripts/fetch-assets.sh" >&2
make -C "$ROOT" build >&2

KERNEL="$ASSETS_DIR/vmlinux-6.1.155"
FC_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH"

log "starting embervm dev"
"$BIN_DIR/embervm" dev \
  --database-url "$DB_URL" \
  --listen ":8080" \
  --insecure-dev-token \
  --plain-root "$WORK_DIR/storage" \
  --netns-pool 4 \
  --script-dir "$ROOT/scripts" \
  --work-dir "$WORK_DIR/work" \
  --kernel "$KERNEL" \
  --fc-bin "$FC_BIN" \
  --uffd-handler "$BIN_DIR/uffd-handler" \
  --guestd-bin "$BIN_DIR/guestd" >"$WORK_DIR/embervm.log" 2>&1 &
EMBERVM_PID=$!

log "waiting for API"
for _ in $(seq 1 30); do
  curl -sf "http://$API/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://$API/healthz" >/dev/null || { cat "$WORK_DIR/embervm.log" >&2; die "API did not come up"; }

log "create template"
TID=$(curl -sf -XPOST "http://$API/v0/templates" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"web","image":"alpine:3.20"}' | jq -r .id)
if [ -z "$TID" ] || [ "$TID" = null ]; then die "template create returned no id"; fi

log "create sandbox"
SID=$(curl -sf -XPOST "http://$API/v0/sandboxes" -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"template_id\":\"$TID\",\"vcpus\":1,\"memory_mib\":256,\"data_disk_gib\":1}" | jq -r .id)
if [ -z "$SID" ] || [ "$SID" = null ]; then die "sandbox create returned no id"; fi

log "exec in sandbox"
OUT=$(curl -sf -XPOST "http://$API/v0/sandboxes/$SID/exec" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo","args":["hello-embervm"]}' | jq -r '.stdout' | base64 -d)
[ "$OUT" = "hello-embervm" ] || die "exec stdout = '$OUT', want 'hello-embervm'"

log "pause + resume"
curl -sf -XPOST "http://$API/v0/sandboxes/$SID/pause"  -H "$AUTH" >/dev/null || die "pause failed"
curl -sf -XPOST "http://$API/v0/sandboxes/$SID/resume" -H "$AUTH" >/dev/null || die "resume failed"

log "exec after resume"
OUT=$(curl -sf -XPOST "http://$API/v0/sandboxes/$SID/exec" -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo","args":["after-resume"]}' | jq -r '.stdout' | base64 -d)
[ "$OUT" = "after-resume" ] || die "post-resume exec stdout = '$OUT'"

log "kill sandbox"
curl -sf -XDELETE "http://$API/v0/sandboxes/$SID" -H "$AUTH" >/dev/null || die "kill failed"

log "PASS: full REST lifecycle through embervm dev"
