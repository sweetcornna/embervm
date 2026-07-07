#!/usr/bin/env bash
#
# restore-matrix.sh -- EmberVM M0 benchmark driver: snapshot-restore latency
# across the (mode x mem_gb x cache) matrix.
#
# Writes one JSON file per cell: $OUT_DIR/restore-<mode>-<mem>g-<cache>.json
# Preconditions (NOT rebuilt here): assets, rootfs, host tool binaries.
# Run test/integration/smoke.sh (and `make build`) first.
#
# Linux-only (Firecracker + KVM); needs root (self-elevates via sudo -E).
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "[restore-matrix.sh] Linux-only (needs KVM + Firecracker); cannot run on $(uname -s)" >&2
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
usage: $(basename "$0") [--modes "file uffd-lazy uffd-prefetch"] [--mems "1 2 4"]
                        [--extra-mems "8"] [--caches "warm cold"] [--iters 15]
                        [--dirty-mb 256] [--out-dir DIR]

Runs the M0 restore-latency matrix. Mem tiers are in GiB; --extra-mems tiers
are best-effort (same headroom guard, skipped with a marker file when the
runner is too small). Requires assets/rootfs/binaries to already exist.
EOF
}

MODES="file uffd-lazy uffd-prefetch"
MEMS="1 2 4"
EXTRA_MEMS="8"
CACHES="warm cold"
ITERS=15
DIRTY_MB=256
OUT_DIR="$RESULTS_DIR"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --modes)      [ "$#" -ge 2 ] || { log "flag --modes requires a value"; usage; exit 2; }
                  MODES="$2"; shift 2 ;;
    --mems)       [ "$#" -ge 2 ] || { log "flag --mems requires a value"; usage; exit 2; }
                  MEMS="$2"; shift 2 ;;
    --extra-mems) [ "$#" -ge 2 ] || { log "flag --extra-mems requires a value"; usage; exit 2; }
                  EXTRA_MEMS="$2"; shift 2 ;;
    --caches)     [ "$#" -ge 2 ] || { log "flag --caches requires a value"; usage; exit 2; }
                  CACHES="$2"; shift 2 ;;
    --iters)      [ "$#" -ge 2 ] || { log "flag --iters requires a value"; usage; exit 2; }
                  ITERS="$2"; shift 2 ;;
    --dirty-mb)   [ "$#" -ge 2 ] || { log "flag --dirty-mb requires a value"; usage; exit 2; }
                  DIRTY_MB="$2"; shift 2 ;;
    --out-dir)    [ "$#" -ge 2 ] || { log "flag --out-dir requires a value"; usage; exit 2; }
                  OUT_DIR="$2"; shift 2 ;;
    -h|--help)    usage; exit 0 ;;
    *)            log "unknown flag: $1"; usage; exit 2 ;;
  esac
done

# --- Preconditions: everything must already exist; we do NOT rebuild here. ---
FC_BIN="$ASSETS_DIR/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH"
missing=0
[ -x "$FC_BIN" ] || { log "missing: $FC_BIN"; missing=1; }
[ -f "$ASSETS_DIR/vmlinux-6.1.155" ] || { log "missing: $ASSETS_DIR/vmlinux-6.1.155"; missing=1; }
[ -f "$ASSETS_DIR/rootfs.ext4" ] || { log "missing: $ASSETS_DIR/rootfs.ext4"; missing=1; }
for b in uffd-handler probe-server probe-client; do
  [ -x "$BIN_DIR/$b" ] || { log "missing: $BIN_DIR/$b"; missing=1; }
done
if [ "$missing" -ne 0 ]; then
  log "preconditions not met -- run test/integration/smoke.sh (assets + rootfs) and 'make build' (binaries) first; this benchmark does not rebuild"
  exit 1
fi

mkdir -p "$OUT_DIR" "$WORK_DIR"

log "environment check (env.json -> $OUT_DIR)"
RESULTS_DIR="$OUT_DIR" bash "$ROOT/scripts/env-check.sh"

log "setup network (id=0, idempotent)"
bash "$ROOT/scripts/setup-network.sh" --id 0

read -r -a MODE_LIST <<<"$MODES"
read -r -a CACHE_LIST <<<"$CACHES"
read -r -a MEM_LIST <<<"$MEMS $EXTRA_MEMS"

for mem_gb in "${MEM_LIST[@]}"; do
  mem_mb=$((mem_gb * 1024))

  # Dirty memory for this tier: cap at 25% of guest RAM.
  dirty="$DIRTY_MB"
  dirty_cap=$((mem_mb / 4))
  if [ "$dirty" -gt "$dirty_cap" ]; then dirty="$dirty_cap"; fi

  # Headroom guard (this is what makes --extra-mems tiers best-effort):
  # RAM:  MemAvailable must exceed guest size + 2GB headroom.
  # Disk: free space on $WORK_DIR must exceed 2.5 x guest size.
  skip_reason=""
  mem_avail_kb=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
  mem_need_kb=$((mem_gb * 1024 * 1024 + 2 * 1024 * 1024))
  disk_avail_kb=$(df -Pk "$WORK_DIR" | awk 'NR==2 {print $4}')
  disk_need_kb=$(awk -v g="$mem_gb" 'BEGIN { printf "%d", g * 2.5 * 1024 * 1024 }')
  if [ "$mem_avail_kb" -le "$mem_need_kb" ]; then
    skip_reason="MemAvailable ${mem_avail_kb}kB <= required ${mem_need_kb}kB (${mem_gb}GB guest + 2GB headroom)"
  elif [ "$disk_avail_kb" -le "$disk_need_kb" ]; then
    skip_reason="free disk on $WORK_DIR ${disk_avail_kb}kB <= required ${disk_need_kb}kB (2.5 x ${mem_gb}GB)"
  fi

  if [ -n "$skip_reason" ]; then
    log "SKIP mem=${mem_gb}g: $skip_reason"
    for mode in "${MODE_LIST[@]}"; do
      for cache in "${CACHE_LIST[@]}"; do
        jq -n \
          --arg mode "$mode" --argjson mem_gb "$mem_gb" --arg cache "$cache" \
          --arg fc "$FC_VERSION" --argjson dirty "$dirty" --arg reason "$skip_reason" \
          '{mode: $mode, mem_gb: $mem_gb, cache: $cache, fc_version: $fc,
            dirty_mb: $dirty, skipped: true, skip_reason: $reason, samples: []}' \
          > "$OUT_DIR/restore-${mode}-${mem_gb}g-${cache}.json"
      done
    done
    continue
  fi

  log "snapshot for tier mem=${mem_gb}g (dirty=${dirty}MiB)"
  bash "$ROOT/scripts/fc-snapshot.sh" --id 0 --mem-mb "$mem_mb" --dirty-mb "$dirty" \
    --out "$WORK_DIR/snap-bench"

  for mode in "${MODE_LIST[@]}"; do
    for cache in "${CACHE_LIST[@]}"; do
      out_file="$OUT_DIR/restore-${mode}-${mem_gb}g-${cache}.json"
      samples_file=$(mktemp)
      log "cell mode=$mode mem=${mem_gb}g cache=$cache: running $ITERS iterations"
      for i in $(seq 1 "$ITERS"); do
        if [ "$cache" = "cold" ]; then
          sync
          echo 3 > /proc/sys/vm/drop_caches
        fi
        line=$(bash "$ROOT/scripts/fc-restore.sh" --id 0 --snap-dir "$WORK_DIR/snap-bench" \
                 --mode "$mode" --iter "$i") \
          || { log "iter $i failed (mode=$mode mem=${mem_gb}g cache=$cache), recording null"; continue; }
        printf '%s\n' "$line" >> "$samples_file"
      done
      jq -n \
        --arg mode "$mode" --argjson mem_gb "$mem_gb" --arg cache "$cache" \
        --arg fc "$FC_VERSION" --argjson dirty "$dirty" \
        --slurpfile samples "$samples_file" \
        '{mode: $mode, mem_gb: $mem_gb, cache: $cache, fc_version: $fc,
          dirty_mb: $dirty, skipped: false, samples: $samples}' \
        > "$out_file"
      rm -f "$samples_file"
      n_ok=$(jq '.samples | length' "$out_file")
      mean_ms=$(jq -r '.samples[].t_interactive_ms' "$out_file" \
        | awk '{ s += $1; n++ } END { if (n > 0) printf "%.1f", s / n; else printf "n/a" }')
      log "cell done mode=$mode mem=${mem_gb}g cache=$cache samples=$n_ok/$ITERS mean_t_interactive_ms=$mean_ms"
    done
  done

  rm -rf "$WORK_DIR/snap-bench"
done

log "restore matrix complete -- results in $OUT_DIR"
