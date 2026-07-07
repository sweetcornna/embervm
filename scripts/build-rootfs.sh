#!/usr/bin/env bash
# shellcheck disable=SC2034  # convention header defines vars not all used by every script
#
# build-rootfs.sh -- build the EmberVM guest rootfs (ext4) from an Ubuntu
# squashfs, baking in probe-server, its systemd unit, and static network
# config for the frozen guest address 172.16.0.2/30.
set -euo pipefail

log() { echo "[$(basename "$0")] $*" >&2; }

# unsquashfs and mkfs.ext4 -d only exist/behave correctly on Linux; fail fast
# on macOS before trying to self-elevate.
[ "$(uname -s)" = Linux ] || { log "linux only"; exit 1; }

if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
WORK_DIR="${WORK_DIR:-$ROOT/work}"
RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"

usage() {
  cat >&2 <<EOF
usage: $(basename "$0") [--squashfs <path>] [--out <path>] [--size-mb <n>] [--probe-server <path>]
defaults:
  --squashfs     \$ASSETS_DIR/ubuntu-24.04.squashfs
  --out          \$ASSETS_DIR/rootfs.ext4
  --size-mb      1536
  --probe-server \$BIN_DIR/probe-server
EOF
}

require_value() {
  if [ "$#" -lt 2 ]; then
    log "flag $1 requires a value"
    usage
    exit 2
  fi
}

SQUASHFS="$ASSETS_DIR/ubuntu-24.04.squashfs"
OUT="$ASSETS_DIR/rootfs.ext4"
SIZE_MB=1536
PROBE_SERVER="$BIN_DIR/probe-server"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --squashfs)
      require_value "$@"
      SQUASHFS="$2"
      shift 2
      ;;
    --out)
      require_value "$@"
      OUT="$2"
      shift 2
      ;;
    --size-mb)
      require_value "$@"
      SIZE_MB="$2"
      shift 2
      ;;
    --probe-server)
      require_value "$@"
      PROBE_SERVER="$2"
      shift 2
      ;;
    *)
      log "unknown flag: $1"
      usage
      exit 2
      ;;
  esac
done

case "$SIZE_MB" in
  '' | *[!0-9]*)
    log "invalid --size-mb (must be a positive integer): $SIZE_MB"
    usage
    exit 2
    ;;
esac

# 1. Prerequisites.
command -v unsquashfs >/dev/null 2>&1 || { log "unsquashfs not found (try: apt-get install squashfs-tools)"; exit 1; }
command -v mkfs.ext4 >/dev/null 2>&1 || { log "mkfs.ext4 not found (try: apt-get install e2fsprogs)"; exit 1; }
[ -f "$SQUASHFS" ] || { log "squashfs image not found: $SQUASHFS"; exit 1; }
[ -x "$PROBE_SERVER" ] || { log "probe-server binary not found: $PROBE_SERVER (try: make build)"; exit 1; }

# 2. Unpack the squashfs into a staging tree.
STAGING="$WORK_DIR/rootfs-build"
rm -rf "$STAGING"
mkdir -p "$STAGING"
log "unpacking $SQUASHFS -> $STAGING/root"
unsquashfs -q -n -d "$STAGING/root" "$SQUASHFS" >&2

# 3. Install probe-server into the guest.
install -D -m 0755 "$PROBE_SERVER" "$STAGING/root/usr/local/bin/probe-server"

# 4. systemd unit, enabled for multi-user.target.
mkdir -p "$STAGING/root/etc/systemd/system"
cat > "$STAGING/root/etc/systemd/system/probe-server.service" <<'EOF'
[Unit]
Description=EmberVM probe server
After=network.target

[Service]
ExecStart=/usr/local/bin/probe-server --addr 0.0.0.0:7777
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF
mkdir -p "$STAGING/root/etc/systemd/system/multi-user.target.wants"
ln -sf ../probe-server.service "$STAGING/root/etc/systemd/system/multi-user.target.wants/probe-server.service"

# 5. Static network config, belt-and-braces: the kernel ip= boot arg does the
# initial config, but if systemd-networkd manages eth0 it must apply the SAME
# static config instead of falling back to DHCP.
mkdir -p "$STAGING/root/etc/systemd/network"
cat > "$STAGING/root/etc/systemd/network/10-eth0.network" <<'EOF'
[Match]
Name=eth0

[Network]
Address=172.16.0.2/30
Gateway=172.16.0.1
DNS=8.8.8.8
EOF
echo "ember" > "$STAGING/root/etc/hostname"

# 6. Build the ext4 image atomically (tmp file + rename).
mkdir -p "$(dirname "$OUT")"
log "creating ${SIZE_MB}MiB ext4 image at $OUT"
truncate -s "${SIZE_MB}M" "$OUT.tmp"
mkfs.ext4 -F -q -d "$STAGING/root" "$OUT.tmp"
mv "$OUT.tmp" "$OUT"

# 7. Cleanup and report.
rm -rf "$STAGING"
log "built $OUT (size: $(du -h "$OUT" | cut -f1))"
