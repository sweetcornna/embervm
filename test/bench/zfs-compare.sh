#!/usr/bin/env bash
#
# zfs-compare.sh -- EmberVM M0 standalone ZFS I/O reference benchmark
# (no Firecracker involved).
#
# Compares fio throughput/latency across ZFS dataset tunables (recordsize x
# primarycache), a zvol, and sync=standard vs sync=disabled -- all on a
# loop-file zpool, so numbers are a functional reference only, not a
# production layout.
#
# Non-blocking by design: if the zfs kernel module cannot be made available
# on the runner, a {"skipped":true,...} marker is written and we exit 0.
#
# Linux-only; needs root (self-elevates via sudo -E).
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "[zfs-compare.sh] Linux-only (needs the zfs kernel module); cannot run on $(uname -s)" >&2
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

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") [--out FILE] [--size-gb N] [--runtime SECONDS]

ZFS tuning-space comparison with fio on a loop-file zpool.
Defaults: --out \$RESULTS_DIR/zfs-compare.json --size-gb 8 --runtime 10
EOF
}

OUT="$RESULTS_DIR/zfs-compare.json"
SIZE_GB=8
RUNTIME=10

while [ "$#" -gt 0 ]; do
  case "$1" in
    --out)     [ "$#" -ge 2 ] || { log "flag --out requires a value"; usage; exit 2; }
               OUT="$2"; shift 2 ;;
    --size-gb) [ "$#" -ge 2 ] || { log "flag --size-gb requires a value"; usage; exit 2; }
               SIZE_GB="$2"; shift 2 ;;
    --runtime) [ "$#" -ge 2 ] || { log "flag --runtime requires a value"; usage; exit 2; }
               RUNTIME="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *)         log "unknown flag: $1"; usage; exit 2 ;;
  esac
done

mkdir -p "$(dirname "$OUT")" "$WORK_DIR"

# --- 1. ZFS module: escalating install fallback chain -----------------------
ensure_zfs() {
  if modprobe zfs 2>/dev/null; then
    log "zfs: module loaded (level 0: already installed on kernel $(uname -r))"
    return 0
  fi
  log "zfs: modprobe failed; level 1: apt-get install zfsutils-linux"
  if apt-get install -y zfsutils-linux >&2 && modprobe zfs 2>/dev/null; then
    log "zfs: module loaded (level 1: zfsutils-linux)"
    return 0
  fi
  log "zfs: still unavailable; level 2: linux-modules-extra-$(uname -r) + zfsutils-linux"
  if apt-get install -y "linux-modules-extra-$(uname -r)" zfsutils-linux >&2 \
      && modprobe zfs 2>/dev/null; then
    log "zfs: module loaded (level 2: linux-modules-extra)"
    return 0
  fi
  log "zfs: still unavailable; level 3: zfs-dkms source build (slow)"
  if DEBIAN_FRONTEND=noninteractive apt-get install -y zfs-dkms zfsutils-linux >&2 \
      && modprobe zfs 2>/dev/null; then
    log "zfs: module loaded (level 3: zfs-dkms)"
    return 0
  fi
  return 1
}

if ! ensure_zfs; then
  log "zfs module unavailable at every level -- writing skip marker to $OUT (exit 0, non-blocking)"
  printf '{"skipped":true,"reason":"zfs module unavailable on runner kernel %s"}\n' \
    "$(uname -r)" > "$OUT"
  cat "$OUT"
  exit 0
fi

# A loadable module does not imply the userland tools: ubuntu-24.04 runners
# ship the zfs kernel module but not zfsutils-linux (zpool/zfs commands).
if ! command -v zpool >/dev/null 2>&1 || ! command -v zfs >/dev/null 2>&1; then
  log "zfs: module ok but zpool/zfs userland missing; installing zfsutils-linux"
  if ! apt-get install -y zfsutils-linux >&2 \
      || ! command -v zpool >/dev/null 2>&1 || ! command -v zfs >/dev/null 2>&1; then
    log "zpool/zfs userland unavailable -- writing skip marker to $OUT (exit 0, non-blocking)"
    printf '{"skipped":true,"reason":"zfsutils-linux userland unavailable on runner"}\n' > "$OUT"
    cat "$OUT"
    exit 0
  fi
fi

# --- 2. Tool requirements ----------------------------------------------------
command -v fio >/dev/null 2>&1 || { log "installing fio"; apt-get install -y fio >&2; }
command -v jq >/dev/null 2>&1 || { log "installing jq"; apt-get install -y jq >&2; }

# --- 3. Loop-file zpool -------------------------------------------------------
POOL="emberbench"
BACKING="$WORK_DIR/zpool-backing.img"
CELLS_DIR="$(mktemp -d)"
FIO_OUT="$(mktemp)"

cleanup() {
  rc=$?
  zpool destroy -f "$POOL" 2>/dev/null || true
  rm -f "$BACKING" "$FIO_OUT"
  rm -rf "$CELLS_DIR"
  rmdir /mnt/eb-* 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT

log "creating ${SIZE_GB}G loop-file zpool '$POOL' on $BACKING (file vdev: functional reference only)"
zpool destroy -f "$POOL" 2>/dev/null || true # leftover pool from an earlier run
truncate -s "${SIZE_GB}G" "$BACKING"
zpool create -f "$POOL" "$BACKING"

# --- fio helpers --------------------------------------------------------------
# run_fio <job-name> <target> <bs> <rw> [extra fio args...]  -> JSON in $FIO_OUT
run_fio() {
  local name="$1" target="$2" bs="$3" rw="$4"
  shift 4
  fio --name="$name" --filename="$target" --size=1G --bs="$bs" --rw="$rw" \
    --ioengine=psync --direct=0 --numjobs=1 --runtime="$RUNTIME" --time_based \
    --ramp_time=2 --group_reporting --output-format=json "$@" > "$FIO_OUT"
}

# wl_params <workload>  -> "bs rw direction"
wl_params() {
  case "$1" in
    randread-4k)   echo "4k randread read" ;;
    randwrite-4k)  echo "4k randwrite write" ;;
    seqread-128k)  echo "128k read read" ;;
    seqwrite-128k) echo "128k write write" ;;
    *) log "internal error: unknown workload $1"; exit 1 ;;
  esac
}

# record_cell <backend> <recordsize> <primarycache> <workload> <direction>
# Extracts metrics from $FIO_OUT into a per-cell JSON file under $CELLS_DIR.
CELL_N=0
record_cell() {
  local backend="$1" rs="$2" pc="$3" wl="$4" dir="$5" cell_file
  CELL_N=$((CELL_N + 1))
  cell_file="$CELLS_DIR/$(printf 'cell-%03d.json' "$CELL_N")"
  jq --arg backend "$backend" --arg rs "$rs" --arg pc "$pc" --arg wl "$wl" --arg dir "$dir" '
    .jobs[0][$dir] as $j
    | {backend: $backend, recordsize: $rs, primarycache: $pc, sync: "standard",
       workload: $wl,
       iops: ($j.iops | round),
       bw_mb_s: (($j.bw_bytes / 1048576 * 10 | round) / 10),
       lat_p99_us: (($j.clat_ns.percentile."99.000000" // 0) / 1000)}
  ' "$FIO_OUT" > "$cell_file"
  log "cell backend=$backend rs=$rs pc=$pc wl=$wl: $(jq -c '{iops, bw_mb_s, lat_p99_us}' "$cell_file")"
}

WORKLOADS=(randread-4k randwrite-4k seqread-128k seqwrite-128k)

# --- 4a. Matrix A: datasets (recordsize x primarycache) -----------------------
log "Matrix A: datasets, recordsize x primarycache"
for rs in 16k 64k 128k; do
  for pc in all metadata; do
    ds="$POOL/ds-$rs-$pc"
    mnt="/mnt/eb-$rs-$pc"
    zfs create -o "recordsize=$rs" -o "primarycache=$pc" -o "mountpoint=$mnt" "$ds"
    for wl in "${WORKLOADS[@]}"; do
      read -r bs rw dir <<<"$(wl_params "$wl")"
      run_fio "$wl" "$mnt/fio-test.dat" "$bs" "$rw"
      record_cell "raw-file" "$rs" "$pc" "$wl" "$dir"
    done
    # Keep pool usage low so the zvol refreservation fits later.
    rm -f "$mnt/fio-test.dat"
  done
done

# --- 4b. Matrix B: zvol (volblocksize=16k) ------------------------------------
log "Matrix B: zvol, volblocksize=16k"
zfs create -V 2G -o volblocksize=16k "$POOL/zv16k"
udevadm settle 2>/dev/null || sleep 2
ZVOL_DEV="/dev/zvol/$POOL/zv16k"
for _ in $(seq 1 20); do
  if [ -e "$ZVOL_DEV" ]; then break; fi
  sleep 0.5
done
[ -e "$ZVOL_DEV" ] || { log "zvol device node $ZVOL_DEV never appeared"; exit 1; }
for wl in "${WORKLOADS[@]}"; do
  read -r bs rw dir <<<"$(wl_params "$wl")"
  run_fio "$wl" "$ZVOL_DEV" "$bs" "$rw"
  record_cell "zvol" "16k" "all" "$wl" "$dir"
done

# --- 5. sync=standard vs sync=disabled ----------------------------------------
log "sync comparison on $POOL/ds-128k-all (randwrite-4k, fsync=1)"
SYNC_DS="$POOL/ds-128k-all"
SYNC_TARGET="/mnt/eb-128k-all/fio-sync.dat"

zfs set sync=standard "$SYNC_DS"
run_fio "sync-standard" "$SYNC_TARGET" 4k randwrite --fsync=1
standard_iops=$(jq '.jobs[0].write.iops | round' "$FIO_OUT")

zfs set sync=disabled "$SYNC_DS"
run_fio "sync-disabled" "$SYNC_TARGET" 4k randwrite --fsync=1
disabled_iops=$(jq '.jobs[0].write.iops | round' "$FIO_OUT")

zfs set sync=standard "$SYNC_DS"

speedup_pct=$(awk -v a="$standard_iops" -v b="$disabled_iops" \
  'BEGIN { if (a > 0) printf "%.1f", (b - a) / a * 100; else printf "0" }')
log "sync compare: standard_iops=$standard_iops disabled_iops=$disabled_iops speedup_pct=$speedup_pct"

SYNC_JSON=$(jq -n \
  --argjson std "$standard_iops" --argjson dis "$disabled_iops" --argjson sp "$speedup_pct" \
  '{workload: "randwrite-4k-fsync", standard_iops: $std, disabled_iops: $dis, speedup_pct: $sp}')

# --- 6. Assemble final output ---------------------------------------------------
zfs_version="$(cat /sys/module/zfs/version 2>/dev/null || zfs version 2>/dev/null | head -n1 || true)"
jq -s \
  --arg kernel "$(uname -r)" \
  --arg zver "$zfs_version" \
  --argjson sync_compare "$SYNC_JSON" \
  '{skipped: false,
    env: {kernel: $kernel, zfs_version: $zver, vdev: "loop-file"},
    cells: .,
    sync_compare: $sync_compare}' \
  "$CELLS_DIR"/cell-*.json > "$OUT"

log "wrote $OUT ($(jq '.cells | length' "$OUT") cells)"
cat "$OUT"
