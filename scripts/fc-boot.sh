#!/usr/bin/env bash
#
# fc-boot.sh — boot a Firecracker microVM inside netns ember$N (EmberVM M0).
#
# Precondition: netns ember$N exists (run scripts/setup-network.sh first).
# Machine-readable output: with --wait-probe, the probe-client JSON line is the
# ONLY thing written to stdout. All logs and progress go to stderr.
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
JAILER_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/jailer-$FC_VERSION-$ARCH"
JAILER_UID="${JAILER_UID:-123}"
JAILER_GID="${JAILER_GID:-100}"

log() { echo "[$(basename "$0")] $*" >&2; }

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") [--id N] [--mem-mb M] [--vcpus V] [--dirty-mb D]
                  [--kernel PATH] [--rootfs PATH] [--jailer] [--wait-probe] [--keep]

Boots a Firecracker microVM inside netns ember\$N.
Defaults: --id 0 --mem-mb 1024 --vcpus 2 --dirty-mb 0
EOF
}

ID=0
MEM_MB=1024
VCPUS=2
DIRTY_MB=0
KERNEL="$ASSETS_DIR/vmlinux-6.1.155"
ROOTFS="$ASSETS_DIR/rootfs.ext4"
JAILER=false
WAIT_PROBE=false
KEEP=false

while [ $# -gt 0 ]; do
  case "$1" in
    --id)         ID="${2:?--id requires a value}"; shift 2 ;;
    --mem-mb)     MEM_MB="${2:?--mem-mb requires a value}"; shift 2 ;;
    --vcpus)      VCPUS="${2:?--vcpus requires a value}"; shift 2 ;;
    --dirty-mb)   DIRTY_MB="${2:?--dirty-mb requires a value}"; shift 2 ;;
    --kernel)     KERNEL="${2:?--kernel requires a value}"; shift 2 ;;
    --rootfs)     ROOTFS="${2:?--rootfs requires a value}"; shift 2 ;;
    --jailer)     JAILER=true; shift ;;
    --wait-probe) WAIT_PROBE=true; shift ;;
    --keep)       KEEP=true; shift ;;
    *)            log "unknown flag: $1"; usage; exit 2 ;;
  esac
done

for v in "$ID" "$MEM_MB" "$VCPUS" "$DIRTY_MB"; do
  [[ "$v" =~ ^[0-9]+$ ]] || { log "expected non-negative integer, got: $v"; usage; exit 2; }
done

# ---------------------------------------------------------------- preconditions
if ! ip netns list | awk -v ns="ember$ID" '$1 == ns { found = 1 } END { exit !found }'; then
  log "netns ember$ID not found — run scripts/setup-network.sh --id $ID first"
  exit 1
fi
[ -x "$FC_BIN" ] || { log "firecracker binary not found: $FC_BIN"; exit 1; }
[ -f "$KERNEL" ] || { log "kernel not found: $KERNEL"; exit 1; }
[ -f "$ROOTFS" ] || { log "rootfs not found: $ROOTFS"; exit 1; }
if [ "$JAILER" = true ]; then
  [ -x "$JAILER_BIN" ] || { log "jailer binary not found: $JAILER_BIN"; exit 1; }
fi

VM_DIR="$WORK_DIR/vm$ID"
rm -rf "$VM_DIR"
mkdir -p "$VM_DIR"

# --------------------------------------------------------------------- helpers
FC_PID=""
JAILER_PIDFILE=""

dump_fc_log() {
  if [ -f "$VM_DIR/fc.log" ]; then
    log "---- fc.log ----"
    cat "$VM_DIR/fc.log" >&2 || true
  fi
}

# With --new-pid-ns the jailer parent exits after writing the real firecracker
# PID to <chroot>/<exec-file-basename>.pid — prefer that PID when present.
refresh_fc_pid() {
  if [ -n "$JAILER_PIDFILE" ] && [ -f "$JAILER_PIDFILE" ]; then
    FC_PID="$(cat "$JAILER_PIDFILE")"
    echo "$FC_PID" > "$VM_DIR/fc.pid"
  fi
}

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
    refresh_fc_pid
    kill_pid_graceful "$FC_PID"
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

api_put() {
  local path="$1" body="$2" code
  code=$(curl -s -o "$VM_DIR/api-resp.json" -w '%{http_code}' --unix-socket "$API_SOCK" \
    -X PUT "http://localhost$path" -H 'Content-Type: application/json' -d "$body")
  if [ "$code" != "204" ]; then
    log "PUT $path failed: HTTP $code $(cat "$VM_DIR/api-resp.json" 2>/dev/null || true)"
    dump_fc_log
    exit 1
  fi
}

# ---------------------------------------------------------------------- launch
log "preparing VM $ID (mem=${MEM_MB}MiB vcpus=$VCPUS dirty=${DIRTY_MB}MiB jailer=$JAILER)"
cp --sparse=always "$ROOTFS" "$VM_DIR/rootfs.ext4"

BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.2::172.16.0.1:255.255.255.252:ember:eth0:off"
if [ "$DIRTY_MB" -gt 0 ]; then
  BOOT_ARGS+=" ember.dirty_mb=$DIRTY_MB"
fi

if [ "$JAILER" = true ]; then
  JAIL_BASE="$WORK_DIR/jail"
  JAIL_VM_DIR="$JAIL_BASE/$(basename "$FC_BIN")/ember$ID"
  CHROOT="$JAIL_VM_DIR/root"
  rm -rf "$JAIL_VM_DIR"
  mkdir -p "$CHROOT/run"
  cp "$KERNEL" "$CHROOT/vmlinux"
  cp --sparse=always "$VM_DIR/rootfs.ext4" "$CHROOT/rootfs.ext4"
  chown -R "$JAILER_UID:$JAILER_GID" "$JAIL_VM_DIR"

  "$JAILER_BIN" --id "ember$ID" --exec-file "$FC_BIN" \
    --uid "$JAILER_UID" --gid "$JAILER_GID" \
    --chroot-base-dir "$JAIL_BASE" --netns "/var/run/netns/ember$ID" \
    --new-pid-ns -- --api-sock /run/fc.sock >"$VM_DIR/fc.log" 2>&1 &
  FC_PID=$!
  echo "$FC_PID" > "$VM_DIR/fc.pid"
  JAILER_PIDFILE="$CHROOT/$(basename "$FC_BIN").pid"
  API_SOCK="$CHROOT/run/fc.sock"
  KERNEL_API_PATH="/vmlinux"
  ROOTFS_API_PATH="/rootfs.ext4"
else
  API_SOCK="$VM_DIR/fc.sock"
  ip netns exec "ember$ID" "$FC_BIN" --api-sock "$API_SOCK" >"$VM_DIR/fc.log" 2>&1 &
  FC_PID=$!
  echo "$FC_PID" > "$VM_DIR/fc.pid"
  KERNEL_API_PATH="$KERNEL"
  ROOTFS_API_PATH="$VM_DIR/rootfs.ext4"
fi

if ! wait_for_socket "$API_SOCK" 100; then # 100 x 50ms = 5s
  log "timed out waiting for API socket $API_SOCK"
  dump_fc_log
  exit 1
fi
refresh_fc_pid

# --------------------------------------------------------------- API sequence
body=$(jq -cn --argjson vcpus "$VCPUS" --argjson mem "$MEM_MB" \
  '{vcpu_count: $vcpus, mem_size_mib: $mem}')
api_put /machine-config "$body"

body=$(jq -cn --arg kernel "$KERNEL_API_PATH" --arg args "$BOOT_ARGS" \
  '{kernel_image_path: $kernel, boot_args: $args}')
api_put /boot-source "$body"

body=$(jq -cn --arg path "$ROOTFS_API_PATH" \
  '{drive_id: "rootfs", path_on_host: $path, is_root_device: true, is_read_only: false}')
api_put /drives/rootfs "$body"

body=$(jq -cn '{iface_id: "eth0", guest_mac: "06:00:AC:10:00:02", host_dev_name: "tap0"}')
api_put /network-interfaces/eth0 "$body"

api_put /actions '{"action_type":"InstanceStart"}'
log "InstanceStart accepted"

# ----------------------------------------------------------------------- probe
if [ "$WAIT_PROBE" = true ]; then
  log "waiting for guest probe on 172.16.0.2:7777"
  if ! PROBE_JSON=$(ip netns exec "ember$ID" "$BIN_DIR/probe-client" --addr 172.16.0.2:7777 --timeout 60s); then
    log "probe-client failed: ${PROBE_JSON:-<no output>}"
    log "---- fc.log (last 50 lines) ----"
    tail -n 50 "$VM_DIR/fc.log" >&2 || true
    exit 1
  fi
  echo "$PROBE_JSON"
fi

# -------------------------------------------------------------------- keep/kill
if [ "$KEEP" = true ]; then
  echo "$API_SOCK" > "$VM_DIR/fc.api"
  log "VM left running: api_sock=$API_SOCK (recorded in $VM_DIR/fc.api) pid=$FC_PID (recorded in $VM_DIR/fc.pid)"
  exit 0
fi

kill_pid_graceful "$FC_PID"
FC_PID=""
log "VM stopped"
exit 0
