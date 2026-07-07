#!/usr/bin/env bash
#
# install.sh -- provision a single EmberVM node on stock Ubuntu 24.04.
#
# Installs PostgreSQL + ZFS userland + e2fsprogs, creates the embervm role/db,
# provisions a ZFS pool (from a real device, or a loopback file for trials),
# fetches Firecracker assets, builds the binaries, and installs a systemd unit
# that runs `embervm dev`. Idempotent; safe to re-run.
#
# It never touches a real disk unless you pass --pool-device: the default is a
# loopback-backed pool under /var/lib/embervm so a trial cannot eat a disk by
# surprise.
set -euo pipefail

log() { echo "[install.sh] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

POOL_NAME="embervm"
POOL_DEVICE=""              # real block device; empty => loopback file
POOL_SIZE_GB="20"          # loopback file size when POOL_DEVICE is empty
DB_URL="postgres:///embervm"
LISTEN=":8080"
NO_SYSTEMD=false

usage() {
  cat >&2 <<EOF
usage: sudo bash install.sh [options]

  --pool-device DEV   use real block device DEV for the ZFS pool (DESTRUCTIVE)
  --pool-size-gb N    loopback pool size in GiB when no device given (default $POOL_SIZE_GB)
  --pool-name NAME    ZFS pool name (default $POOL_NAME)
  --database-url URL  PostgreSQL URL (default $DB_URL)
  --listen ADDR       API listen address (default $LISTEN)
  --no-systemd        do not install/enable the systemd unit
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --pool-device)  POOL_DEVICE="${2:?}"; shift 2 ;;
    --pool-size-gb) POOL_SIZE_GB="${2:?}"; shift 2 ;;
    --pool-name)    POOL_NAME="${2:?}"; shift 2 ;;
    --database-url) DB_URL="${2:?}"; shift 2 ;;
    --listen)       LISTEN="${2:?}"; shift 2 ;;
    --no-systemd)   NO_SYSTEMD=true; shift ;;
    -h|--help)      usage; exit 0 ;;
    *)              usage; die "unknown flag: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "must run as root (sudo)"
[ "$(uname -s)" = Linux ] || die "Linux only"
[ -w /dev/kvm ] || log "WARNING: /dev/kvm not writable — the node will not boot microVMs until KVM is available"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
STATE_DIR="/var/lib/embervm"
mkdir -p "$STATE_DIR"

# 1. Packages.
log "installing packages (postgresql, zfsutils-linux, e2fsprogs, iproute2, curl, make, golang)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y >&2
apt-get install -y postgresql zfsutils-linux e2fsprogs iproute2 curl make golang-go >&2

# 2. PostgreSQL role + database (idempotent).
log "ensuring postgres role/db 'embervm'"
systemctl enable --now postgresql >&2 || true
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='embervm'" | grep -q 1 \
  || sudo -u postgres psql -c "CREATE ROLE embervm LOGIN PASSWORD 'embervm'" >&2
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='embervm'" | grep -q 1 \
  || sudo -u postgres createdb -O embervm embervm >&2

# 3. ZFS pool (idempotent).
if zpool list "$POOL_NAME" >/dev/null 2>&1; then
  log "zfs pool '$POOL_NAME' already exists"
elif [ -n "$POOL_DEVICE" ]; then
  log "creating zfs pool '$POOL_NAME' on device $POOL_DEVICE (DESTRUCTIVE)"
  zpool create -f "$POOL_NAME" "$POOL_DEVICE" >&2
else
  IMG="$STATE_DIR/pool.img"
  log "no --pool-device: creating a ${POOL_SIZE_GB}GiB LOOPBACK pool at $IMG (trial use only)"
  [ -f "$IMG" ] || truncate -s "${POOL_SIZE_GB}G" "$IMG"
  zpool create -f "$POOL_NAME" "$IMG" >&2
fi

# 4. Build binaries + fetch assets.
log "fetching Firecracker assets"
bash "$ROOT/scripts/fetch-assets.sh" >&2
log "building EmberVM binaries"
make -C "$ROOT" build >&2

FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"
KERNEL="$ROOT/assets/vmlinux-6.1.155"
FC_BIN="$ROOT/assets/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH"

# 5. systemd unit.
if [ "$NO_SYSTEMD" = false ]; then
  UNIT=/etc/systemd/system/embervm.service
  log "installing systemd unit $UNIT"
  cat > "$UNIT" <<EOF
[Unit]
Description=EmberVM single-node (embervm dev)
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
ExecStart=$ROOT/bin/embervm dev \\
  --database-url $DB_URL \\
  --listen $LISTEN \\
  --zfs-pool $POOL_NAME \\
  --script-dir $ROOT/scripts \\
  --work-dir $STATE_DIR/work \\
  --kernel $KERNEL \\
  --fc-bin $FC_BIN \\
  --uffd-handler $ROOT/bin/uffd-handler \\
  --guestd-bin $ROOT/bin/guestd
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable embervm.service >&2
  log "start it with: systemctl start embervm"
else
  log "skipping systemd (--no-systemd); run manually:"
  echo "sudo $ROOT/bin/embervm dev --database-url $DB_URL --listen $LISTEN --zfs-pool $POOL_NAME --script-dir $ROOT/scripts --work-dir $STATE_DIR/work --kernel $KERNEL --fc-bin $FC_BIN --uffd-handler $ROOT/bin/uffd-handler --guestd-bin $ROOT/bin/guestd" >&2
fi

log "done. Dev token: 'dev-token'. Try:"
echo "  curl -sXPOST localhost${LISTEN}/v0/templates -H 'Authorization: Bearer dev-token' -H 'Content-Type: application/json' -d '{\"name\":\"web\",\"image\":\"alpine:3.20\"}'" >&2
