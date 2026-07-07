#!/usr/bin/env bash
set -euo pipefail

log() { echo "[$(basename "$0")] $*" >&2; }

# SAFETY GUARD: this script deletes system directories to reclaim disk space
# on GitHub Actions ubuntu runners. It must NEVER run on a dev machine.
if [ "${GITHUB_ACTIONS:-}" != "true" ]; then
  log "refusing to run outside GitHub Actions"
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
WORK_DIR="${WORK_DIR:-$ROOT/work}"
RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
# Convention header above; reference every var so shellcheck (SC2034) stays quiet.
: "$ASSETS_DIR" "$WORK_DIR" "$RESULTS_DIR" "$BIN_DIR" "$FC_VERSION" "$ARCH"

# avail_kb <path> -> available KB on the filesystem holding <path> (empty if absent)
avail_kb() {
  df -Pk "$1" 2>/dev/null | awk 'NR==2 {print $4}' || true
}

# show_df <label> -> human-readable df of / and /mnt, to stderr
show_df() {
  log "disk usage ($1):"
  df -h / >&2 || true
  if [ -d /mnt ]; then
    df -h /mnt >&2 || true
  fi
}

show_df "before"
before_root="$(avail_kb /)"
before_root="${before_root:-0}"
before_mnt="$(avail_kb /mnt)"
before_mnt="${before_mnt:-0}"

targets=(
  /usr/share/dotnet
  /usr/local/lib/android
  /opt/ghc
  /usr/local/.ghcup
  /opt/hostedtoolcache/CodeQL
  /usr/share/swift
  /usr/local/share/boost
)
for t in "${targets[@]}"; do
  log "removing $t"
  rm -rf "$t" || true
done

log "pruning docker images"
docker image prune -af >&2 || true

show_df "after"
after_root="$(avail_kb /)"
after_root="${after_root:-0}"
after_mnt="$(avail_kb /mnt)"
after_mnt="${after_mnt:-0}"

freed_gb="$(awk -v br="$before_root" -v bm="$before_mnt" \
              -v ar="$after_root" -v am="$after_mnt" \
  'BEGIN { printf "%.1f", ((ar - br) + (am - bm)) / 1048576 }')"
log "freed $freed_gb GB"
