#!/usr/bin/env bash
# shellcheck disable=SC2034  # convention header defines vars not all used by every script
#
# setup-network.sh -- create the per-id EmberVM network sandbox (M0).
#
# Layout for --id N:
#   netns emberN: tap0 172.16.0.1/30 (guest is always 172.16.0.2),
#                 veth peer vpN 10.200.N.2/30, default route via 10.200.N.1
#   root ns:      veth veN 10.200.N.1/30, NAT + FORWARD rules tagged "embervm-N"
#
# NOTE: no NOTRACK rules in M0 -- conntrack bypass is a later optimization.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then exec sudo -E bash "$0" "$@"; fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS_DIR="${ASSETS_DIR:-$ROOT/assets}"
WORK_DIR="${WORK_DIR:-$ROOT/work}"
RESULTS_DIR="${RESULTS_DIR:-$ROOT/results}"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
ARCH="${ARCH:-x86_64}"

log() { echo "[$(basename "$0")] $*" >&2; }

usage() {
  echo "usage: $(basename "$0") [--id N]" >&2
}

ID=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --id)
      [ "$#" -ge 2 ] || { log "flag --id requires a value"; usage; exit 2; }
      ID="$2"
      shift 2
      ;;
    *)
      log "unknown flag: $1"
      usage
      exit 2
      ;;
  esac
done

case "$ID" in
  '' | *[!0-9]*)
    log "invalid --id (must be a non-negative integer): $ID"
    usage
    exit 2
    ;;
esac

NS="ember$ID"
VE="ve$ID"
VP="vp$ID"
HOST_IP="10.200.$ID.1"
NS_IP="10.200.$ID.2"
VETH_NET="10.200.$ID.0/30"

# Idempotency: clear any previous state for this id first.
"$ROOT/scripts/teardown-network.sh" --id "$ID" || true

# 1-2. Namespace with loopback up.
ip netns add "$NS"
ip netns exec "$NS" ip link set lo up

# 3. TAP device for the guest, inside the namespace.
ip netns exec "$NS" ip tuntap add dev tap0 mode tap
ip netns exec "$NS" ip addr add 172.16.0.1/30 dev tap0
ip netns exec "$NS" ip link set tap0 up

# 4. veth pair bridging root ns <-> namespace, plus default route inside the ns.
ip link add "$VE" type veth peer name "$VP"
ip link set "$VP" netns "$NS"
ip addr add "$HOST_IP/30" dev "$VE"
ip link set "$VE" up
ip netns exec "$NS" ip addr add "$NS_IP/30" dev "$VP"
ip netns exec "$NS" ip link set "$VP" up
ip netns exec "$NS" ip route add default via "$HOST_IP"

# 5. IPv4 forwarding in both namespaces.
sysctl -q -w net.ipv4.ip_forward=1
ip netns exec "$NS" sysctl -q -w net.ipv4.ip_forward=1

# 6. NAT: guest net -> veth inside the ns; veth net -> world in the root ns.
#    Root-ns rules are tagged "embervm-$ID" so teardown can remove them precisely.
ip netns exec "$NS" iptables -t nat -A POSTROUTING -s 172.16.0.0/30 -o "$VP" -j MASQUERADE
iptables -t nat -A POSTROUTING -s "$VETH_NET" -j MASQUERADE -m comment --comment "embervm-$ID"
iptables -A FORWARD -s "$VETH_NET" -j ACCEPT -m comment --comment "embervm-$ID"
iptables -A FORWARD -d "$VETH_NET" -j ACCEPT -m comment --comment "embervm-$ID"

log "network ready: ns=$NS tap0=172.16.0.1/30 guest=172.16.0.2 veth $VE($HOST_IP) <-> $VP($NS_IP)"
