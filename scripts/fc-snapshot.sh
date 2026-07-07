#!/usr/bin/env bash
#
# fc-snapshot.sh — boot a microVM via fc-boot.sh, pause it, take a Full
# snapshot into --out DIR, then kill the VM (EmberVM M0).
#
# Produces $OUT/{snapfile,memfile,meta.json}. The snapshot represents the
# PAUSED state: the VM is never resumed after /snapshot/create.
# No machine-readable stdout output; all logs go to stderr.
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

log() { echo "[$(basename "$0")] $*" >&2; }

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") [--id N] [--mem-mb M] [--vcpus V] [--dirty-mb D] [--out DIR]

Boots, pauses, and snapshots a microVM into DIR (default \$WORK_DIR/snap\$N).
Defaults: --id 0 --mem-mb 1024 --vcpus 2 --dirty-mb 128
EOF
}

ID=0
MEM_MB=1024
VCPUS=2
DIRTY_MB=128
OUT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --id)       ID="${2:?--id requires a value}"; shift 2 ;;
    --mem-mb)   MEM_MB="${2:?--mem-mb requires a value}"; shift 2 ;;
    --vcpus)    VCPUS="${2:?--vcpus requires a value}"; shift 2 ;;
    --dirty-mb) DIRTY_MB="${2:?--dirty-mb requires a value}"; shift 2 ;;
    --out)      OUT="${2:?--out requires a value}"; shift 2 ;;
    *)          log "unknown flag: $1"; usage; exit 2 ;;
  esac
done

for v in "$ID" "$MEM_MB" "$VCPUS" "$DIRTY_MB"; do
  [[ "$v" =~ ^[0-9]+$ ]] || { log "expected non-negative integer, got: $v"; usage; exit 2; }
done

OUT="${OUT:-$WORK_DIR/snap$ID}"
VM_DIR="$WORK_DIR/vm$ID"

# --------------------------------------------------------------------- helpers
FC_PID=""

kill_pid_graceful() { # TERM, wait up to 3s, then KILL
  local pid="$1" i
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for ((i = 0; i < 30; i++)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.1
  done
  kill -KILL "$pid" 2>/dev/null || true
}

# Invoked indirectly via the EXIT trap below.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    kill_pid_graceful "$FC_PID"
  fi
}
trap cleanup EXIT

api_call() {
  local method="$1" path="$2" body="$3" code
  code=$(curl -s -o "$VM_DIR/api-resp.json" -w '%{http_code}' --unix-socket "$API_SOCK" \
    -X "$method" "http://localhost$path" -H 'Content-Type: application/json' -d "$body")
  if [ "$code" != "204" ]; then
    log "$method $path failed: HTTP $code $(cat "$VM_DIR/api-resp.json" 2>/dev/null || true)"
    if [ -f "$VM_DIR/fc.log" ]; then
      log "---- fc.log ----"
      cat "$VM_DIR/fc.log" >&2 || true
    fi
    exit 1
  fi
}

# ------------------------------------------------------------------------ main
# 1. Fresh snapshot output dir (absolute, so Firecracker resolves it correctly).
rm -rf "$OUT"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

# 2. Boot the VM and wait until the guest probe answers.
log "booting VM $ID (mem=${MEM_MB}MiB vcpus=$VCPUS dirty=${DIRTY_MB}MiB)"
PROBE_JSON=$(bash "$ROOT/scripts/fc-boot.sh" --id "$ID" --mem-mb "$MEM_MB" \
  --vcpus "$VCPUS" --dirty-mb "$DIRTY_MB" --wait-probe --keep)
log "probe: $PROBE_JSON"

SEQ=$(jq -r '.seq' <<<"$PROBE_JSON")
[[ "$SEQ" =~ ^[0-9]+$ ]] || { log "could not parse .seq from probe output: $PROBE_JSON"; exit 1; }

API_SOCK=$(cat "$VM_DIR/fc.api")
FC_PID=$(cat "$VM_DIR/fc.pid")

# 3. Pause the VM.
log "pausing VM"
api_call PATCH /vm '{"state":"Paused"}'

# 4. Full snapshot. Never resume afterwards — the snapshot must represent the
#    paused state; the VM is killed directly below.
log "creating snapshot in $OUT"
body=$(jq -cn --arg snap "$OUT/snapfile" --arg mem "$OUT/memfile" \
  '{snapshot_type: "Full", snapshot_path: $snap, mem_file_path: $mem}')
api_call PUT /snapshot/create "$body"

# 5. Kill the paused VM.
kill_pid_graceful "$FC_PID"
FC_PID=""

# 6. Preserve the drive backing file. The snapfile records the drive's
#    absolute host path from snapshot time; every restore must recreate a
#    pristine copy at exactly that path (fc-restore.sh wipes the VM dir).
DRIVE_PATH="$VM_DIR/rootfs.ext4"
cp --sparse=always "$DRIVE_PATH" "$OUT/rootfs.ext4"

# 7. Metadata for consumers (fc-restore.sh asserts seq_at_snapshot + 1 and
#    restores the rootfs copy to drive_path before loading).
jq -n --argjson mem_mb "$MEM_MB" --argjson vcpus "$VCPUS" --argjson dirty_mb "$DIRTY_MB" \
  --arg fc_version "$FC_VERSION" --argjson seq "$SEQ" --arg drive_path "$DRIVE_PATH" \
  '{mem_mb: $mem_mb, vcpus: $vcpus, dirty_mb: $dirty_mb, fc_version: $fc_version,
    seq_at_snapshot: $seq, drive_path: $drive_path}' \
  > "$OUT/meta.json"

# 8. Report sizes.
log "snapshot artifact sizes:"
du -h "$OUT/snapfile" "$OUT/memfile" "$OUT/rootfs.ext4" >&2
log "meta.json: $(cat "$OUT/meta.json")"
log "done"
exit 0
