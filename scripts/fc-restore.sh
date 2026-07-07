#!/usr/bin/env bash
#
# fc-restore.sh — restore a microVM from a snapshot and measure latency
# (EmberVM M0).
#
# Modes:
#   file          memory restored from memfile via mmap (Firecracker "File")
#   uffd-lazy     userfaultfd backend, uffd-handler serves faults on demand
#   uffd-prefetch userfaultfd backend, uffd-handler prefetches memory
#
# Emits EXACTLY ONE JSON line on stdout:
#   {"mode":..,"iter":..,"t_api_ms":..,"t_interactive_ms":..,"seq":..,
#    "seq_ok":..,"faults_served":..,"bytes_copied_fault":..,"bytes_copied_prefetch":..}
# All logs go to stderr. Exit 0 iff the whole sequence succeeded and seq
# parsed; seq_ok=false alone does NOT fail the script (callers assert).
set -euo pipefail

[ "$(uname -s)" = Linux ] || { echo "linux only" >&2; exit 1; }
if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
export WORK_DIR="${WORK_DIR:-$ROOT/work}"
export RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
export BIN_DIR="${BIN_DIR:-$ROOT/bin}"
export FC_VERSION="${FC_VERSION:-v1.16.1}"
export ARCH="${ARCH:-x86_64}"

FC_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH"

log() { echo "[$(basename "$0")] $*" >&2; }

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") --mode file|uffd-lazy|uffd-prefetch [--id N] [--snap-dir DIR] [--iter I]

Restores a microVM from DIR (default \$WORK_DIR/snap\$N) inside netns ember\$N
and prints one JSON result line to stdout.
EOF
}

ID=0
SNAP=""
MODE=""
ITER=0

while [ $# -gt 0 ]; do
  case "$1" in
    --id)       ID="${2:?--id requires a value}"; shift 2 ;;
    --snap-dir) SNAP="${2:?--snap-dir requires a value}"; shift 2 ;;
    --mode)     MODE="${2:?--mode requires a value}"; shift 2 ;;
    --iter)     ITER="${2:?--iter requires a value}"; shift 2 ;;
    *)          log "unknown flag: $1"; usage; exit 2 ;;
  esac
done

case "$MODE" in
  file|uffd-lazy|uffd-prefetch) ;;
  *) log "--mode is required and must be file|uffd-lazy|uffd-prefetch (got: '$MODE')"; usage; exit 2 ;;
esac
for v in "$ID" "$ITER"; do
  [[ "$v" =~ ^[0-9]+$ ]] || { log "expected non-negative integer, got: $v"; usage; exit 2; }
done
SNAP="${SNAP:-$WORK_DIR/snap$ID}"

# ---------------------------------------------------------------- preconditions
if ! ip netns list | awk -v ns="ember$ID" '$1 == ns { found = 1 } END { exit !found }'; then
  log "netns ember$ID not found — run scripts/setup-network.sh --id $ID first"
  exit 1
fi
[ -x "$FC_BIN" ] || { log "firecracker binary not found: $FC_BIN"; exit 1; }
for f in snapfile memfile meta.json; do
  [ -f "$SNAP/$f" ] || { log "missing $SNAP/$f — run scripts/fc-snapshot.sh first"; exit 1; }
done
SNAP="$(cd "$SNAP" && pwd)"

SEQ_AT_SNAPSHOT=$(jq -r '.seq_at_snapshot' "$SNAP/meta.json")
[[ "$SEQ_AT_SNAPSHOT" =~ ^[0-9]+$ ]] || { log "bad seq_at_snapshot in $SNAP/meta.json"; exit 1; }

VM_DIR="$WORK_DIR/vm$ID"
rm -rf "$VM_DIR"
mkdir -p "$VM_DIR"
API_SOCK="$VM_DIR/fc.sock"

# The snapfile records the drive's absolute host path from snapshot time;
# recreate a pristine copy there so (a) the path exists and (b) disk writes
# from earlier restore iterations cannot leak into this one.
DRIVE_PATH=$(jq -r '.drive_path // empty' "$SNAP/meta.json")
[ -n "$DRIVE_PATH" ] || DRIVE_PATH="$WORK_DIR/vm$ID/rootfs.ext4"
[ -f "$SNAP/rootfs.ext4" ] || { log "missing $SNAP/rootfs.ext4 — re-run scripts/fc-snapshot.sh"; exit 1; }
mkdir -p "$(dirname "$DRIVE_PATH")"
cp --sparse=always "$SNAP/rootfs.ext4" "$DRIVE_PATH"

# --------------------------------------------------------------------- helpers
FC_PID=""
UFFD_PID=""

dump_logs() {
  local f
  for f in "$VM_DIR/fc.log" "$VM_DIR/uffd.log"; do
    if [ -f "$f" ]; then
      log "---- $(basename "$f") ----"
      cat "$f" >&2 || true
    fi
  done
}

wait_pid_gone() { # pid, max-deciseconds
  local pid="$1" max="$2" i
  for ((i = 0; i < max; i++)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.1
  done
  return 1
}

kill_pid_graceful() { # TERM, wait up to 3s, then KILL
  local pid="$1"
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  wait_pid_gone "$pid" 30 || kill -KILL "$pid" 2>/dev/null || true
}

# Invoked indirectly via the EXIT trap below.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    kill_pid_graceful "$FC_PID"
    kill_pid_graceful "$UFFD_PID"
  fi
}
trap cleanup EXIT

wait_for_socket() {
  local sock="$1" max_iter="$2" i
  for ((i = 0; i < max_iter; i++)); do
    if [ -S "$sock" ]; then return 0; fi
    sleep 0.05
  done
  return 1
}

# ------------------------------------------------------------- 1. firecracker
log "starting firecracker for restore (mode=$MODE iter=$ITER snap=$SNAP)"
ip netns exec "ember$ID" "$FC_BIN" --api-sock "$API_SOCK" >"$VM_DIR/fc.log" 2>&1 &
FC_PID=$!
echo "$FC_PID" > "$VM_DIR/fc.pid"

if ! wait_for_socket "$API_SOCK" 100; then # 100 x 50ms = 5s
  log "timed out waiting for API socket $API_SOCK"
  dump_logs
  exit 1
fi

# ------------------------------------------------------------- 2. uffd handler
UFFD_SOCK="$VM_DIR/uffd.sock"
if [ "$MODE" != "file" ]; then
  HMODE="${MODE#uffd-}" # lazy | prefetch
  [ -x "$BIN_DIR/uffd-handler" ] || { log "uffd-handler not found: $BIN_DIR/uffd-handler (run make build)"; exit 1; }
  log "starting uffd-handler (mode=$HMODE)"
  "$BIN_DIR/uffd-handler" --socket "$UFFD_SOCK" --memfile "$SNAP/memfile" \
    --mode "$HMODE" --metrics-out "$VM_DIR/uffd-metrics.json" \
    >"$VM_DIR/uffd.log" 2>&1 &
  UFFD_PID=$!
  if ! wait_for_socket "$UFFD_SOCK" 40; then # 40 x 50ms = 2s
    log "timed out waiting for uffd socket $UFFD_SOCK"
    dump_logs
    exit 1
  fi
fi

# ----------------------------------------------------------- 3. snapshot load
if [ "$MODE" = "file" ]; then
  body=$(jq -cn --arg snap "$SNAP/snapfile" --arg mem "$SNAP/memfile" \
    '{snapshot_path: $snap, mem_backend: {backend_type: "File", backend_path: $mem}, resume_vm: true}')
else
  body=$(jq -cn --arg snap "$SNAP/snapfile" --arg sock "$UFFD_SOCK" \
    '{snapshot_path: $snap, mem_backend: {backend_type: "Uffd", backend_path: $sock}, resume_vm: true}')
fi

T0=$(date +%s%N)
code=$(curl -s -o "$VM_DIR/api-resp.json" -w '%{http_code}' --unix-socket "$API_SOCK" \
  -X PUT "http://localhost/snapshot/load" -H 'Content-Type: application/json' -d "$body")
if [ "$code" != "204" ]; then
  log "PUT /snapshot/load failed: HTTP $code $(cat "$VM_DIR/api-resp.json" 2>/dev/null || true)"
  dump_logs
  exit 1
fi
T1=$(date +%s%N)

# -------------------------------------------------------------------- 4. probe
if ! PROBE_JSON=$(ip netns exec "ember$ID" "$BIN_DIR/probe-client" --addr 172.16.0.2:7777 --timeout 60s); then
  log "probe-client failed after restore: ${PROBE_JSON:-<no output>}"
  dump_logs
  exit 1
fi
log "probe: $PROBE_JSON"
T2=$(jq -r '.success_unix_ns' <<<"$PROBE_JSON")
SEQ=$(jq -r '.seq' <<<"$PROBE_JSON")
[[ "$T2" =~ ^[0-9]+$ ]] || { log "bad success_unix_ns in probe output: $PROBE_JSON"; exit 1; }
[[ "$SEQ" =~ ^[0-9]+$ ]] || { log "bad seq in probe output: $PROBE_JSON"; exit 1; }

# ----------------------------------------------------------------- 5. teardown
kill_pid_graceful "$FC_PID"
FC_PID=""

FAULTS_SERVED=0
BYTES_COPIED_FAULT=0
BYTES_COPIED_PREFETCH=0
if [ -n "$UFFD_PID" ]; then
  kill -TERM "$UFFD_PID" 2>/dev/null || true
  if ! wait_pid_gone "$UFFD_PID" 50; then # it writes metrics on shutdown; wait up to 5s
    log "uffd-handler did not exit within 5s after SIGTERM; killing"
    kill -KILL "$UFFD_PID" 2>/dev/null || true
  fi
  UFFD_PID=""
  if [ -f "$VM_DIR/uffd-metrics.json" ]; then
    FAULTS_SERVED=$(jq -r '.faults_served // 0' "$VM_DIR/uffd-metrics.json")
    BYTES_COPIED_FAULT=$(jq -r '.bytes_copied_fault // 0' "$VM_DIR/uffd-metrics.json")
    BYTES_COPIED_PREFETCH=$(jq -r '.bytes_copied_prefetch // 0' "$VM_DIR/uffd-metrics.json")
  else
    log "warning: uffd metrics file missing: $VM_DIR/uffd-metrics.json"
  fi
fi

# ---------------------------------------------------------------- 6. seq check
SEQ_EXPECTED=$((SEQ_AT_SNAPSHOT + 1))
if [ "$SEQ" -eq "$SEQ_EXPECTED" ]; then
  SEQ_OK=true
else
  SEQ_OK=false
  log "seq mismatch: got $SEQ, expected $SEQ_EXPECTED"
fi

# ------------------------------------------------------------------- 7. result
T_API_MS=$(awk -v d="$((T1 - T0))" 'BEGIN { printf "%.3f", d / 1e6 }')
T_INTERACTIVE_MS=$(awk -v d="$((T2 - T0))" 'BEGIN { printf "%.3f", d / 1e6 }')

jq -cn \
  --arg mode "$MODE" \
  --argjson iter "$ITER" \
  --argjson t_api_ms "$T_API_MS" \
  --argjson t_interactive_ms "$T_INTERACTIVE_MS" \
  --argjson seq "$SEQ" \
  --argjson seq_ok "$SEQ_OK" \
  --argjson faults_served "$FAULTS_SERVED" \
  --argjson bytes_copied_fault "$BYTES_COPIED_FAULT" \
  --argjson bytes_copied_prefetch "$BYTES_COPIED_PREFETCH" \
  '{mode: $mode, iter: $iter, t_api_ms: $t_api_ms, t_interactive_ms: $t_interactive_ms,
    seq: $seq, seq_ok: $seq_ok, faults_served: $faults_served,
    bytes_copied_fault: $bytes_copied_fault, bytes_copied_prefetch: $bytes_copied_prefetch}'
exit 0
