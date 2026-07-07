#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
WORK_DIR="${WORK_DIR:-$ROOT/work}"
RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
# Convention header above; reference every var so shellcheck (SC2034) stays quiet.
: "$ASSETS_DIR" "$WORK_DIR" "$RESULTS_DIR" "$BIN_DIR" "$FC_VERSION" "$ARCH"

log() { echo "[$(basename "$0")] $*" >&2; }

LOCKFILE="$ROOT/scripts/assets.sha256"
CI_ASSET_BASE="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/$ARCH"

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") [--update-lock] [--versions "<v1 v2 ...>"]

Download and sha256-verify the pinned Firecracker release(s) and guest
assets into \$ASSETS_DIR ($ASSETS_DIR), guarded by the lockfile
scripts/assets.sha256 ("<sha256>  <basename>", shasum -c compatible).

  --update-lock          append missing lockfile entries instead of failing
  --versions "<list>"    space-separated Firecracker versions (default: \$FC_VERSION)
EOF
}

UPDATE_LOCK=0
VERSIONS="$FC_VERSION"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --update-lock)
      UPDATE_LOCK=1
      shift
      ;;
    --versions)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      VERSIONS="$2"
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{print $1}'
}

# lock_get <basename> -> prints locked hash, empty if no entry
lock_get() {
  [ -f "$LOCKFILE" ] || return 0
  awk -v f="$1" '$2 == f { print $1; exit }' "$LOCKFILE"
}

# verify_or_record <file-path>
# Verify the file against its lockfile entry; hard-fail on mismatch.
# Missing entry: append with --update-lock, otherwise fail with guidance.
verify_or_record() {
  local file="$1" name have want
  name="$(basename "$file")"
  have="$(sha256 "$file")"
  want="$(lock_get "$name")"
  if [ -n "$want" ]; then
    if [ "$have" != "$want" ]; then
      log "sha256 MISMATCH for $name"
      log "  lockfile: $want"
      log "  actual:   $have"
      exit 1
    fi
    log "verified $name"
  elif [ "$UPDATE_LOCK" -eq 1 ]; then
    printf '%s  %s\n' "$have" "$name" >> "$LOCKFILE"
    sort -k2 -o "$LOCKFILE" "$LOCKFILE"
    log "recorded $name ($have) in $LOCKFILE"
  else
    log "no lockfile entry for $name in $LOCKFILE; re-run with --update-lock to record it"
    exit 1
  fi
}

# download <url> <dest>
download() {
  local url="$1" dest="$2" tmp
  tmp="$dest.tmp.$$"
  log "downloading $url"
  curl -fSL --retry 3 --retry-delay 2 -o "$tmp" "$url"
  mv "$tmp" "$dest"
}

# fetch_fc_version <version>
fetch_fc_version() {
  local v="$1" tgz_name tgz_path fc_bin url want
  tgz_name="firecracker-$v-$ARCH.tgz"
  tgz_path="$ASSETS_DIR/$tgz_name"
  fc_bin="$ASSETS_DIR/release-$v-$ARCH/firecracker-$v-$ARCH"
  url="https://github.com/firecracker-microvm/firecracker/releases/download/$v/$tgz_name"

  if [ -x "$fc_bin" ] && [ -f "$tgz_path" ]; then
    want="$(lock_get "$tgz_name")"
    if [ -n "$want" ] && [ "$(sha256 "$tgz_path")" = "$want" ]; then
      log "firecracker $v: binary present and tgz matches lockfile; skipping"
      return 0
    fi
  fi

  [ -f "$tgz_path" ] || download "$url" "$tgz_path"
  verify_or_record "$tgz_path"
  log "extracting $tgz_name into $ASSETS_DIR"
  tar xzf "$tgz_path" -C "$ASSETS_DIR"
  if [ ! -x "$fc_bin" ]; then
    log "expected binary missing after extraction: $fc_bin"
    exit 1
  fi
}

# fetch_static <url>  (version-independent asset, downloaded once)
fetch_static() {
  local url="$1" name path
  name="$(basename "$url")"
  path="$ASSETS_DIR/$name"
  [ -f "$path" ] || download "$url" "$path"
  verify_or_record "$path"
}

mkdir -p "$ASSETS_DIR"

if [ ! -f "$LOCKFILE" ]; then
  if [ "$UPDATE_LOCK" -eq 1 ]; then
    : > "$LOCKFILE"
    log "created new lockfile $LOCKFILE"
  else
    log "lockfile $LOCKFILE not found; run with --update-lock to create it"
    exit 1
  fi
fi

read -r -a version_list <<< "$VERSIONS"
for v in "${version_list[@]}"; do
  fetch_fc_version "$v"
done

fetch_static "$CI_ASSET_BASE/vmlinux-6.1.155"
fetch_static "$CI_ASSET_BASE/ubuntu-24.04.squashfs"

log "all assets present and verified in $ASSETS_DIR"
