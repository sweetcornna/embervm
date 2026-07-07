#!/usr/bin/env bash
# shellcheck disable=SC2034  # convention header defines vars not all used by every script
#
# teardown-network.sh -- remove the per-id EmberVM network sandbox (M0).
# Fully tolerant of partial or absent state; safe to run repeatedly.
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

# Remove root-ns iptables rules tagged "embervm-$ID". iptables-save quotes the
# comment value, so match the quoted form -- this keeps id 1 from matching
# embervm-10, embervm-11, ...
for table in nat filter; do
  while IFS= read -r line; do
    # Our comment tags contain no whitespace, so dropping the quotes that
    # iptables-save adds lets plain word splitting reconstruct the rule spec.
    line="${line//\"/}"
    rule="${line#-A }"
    # shellcheck disable=SC2086  # intentional word splitting of the rule spec
    iptables -t "$table" -D $rule 2>/dev/null || true
  done < <(iptables-save -t "$table" 2>/dev/null | grep -F -- "--comment \"embervm-$ID\"" | grep '^-A ' || true)
done

# Deleting the namespace removes tap0 and vpN (and the in-ns iptables rules).
ip link del "$VE" 2>/dev/null || true
ip netns del "$NS" 2>/dev/null || true

log "teardown complete: id=$ID (ns=$NS, ve=$VE, root-ns rules tagged embervm-$ID removed)"
