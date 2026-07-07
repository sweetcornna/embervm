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

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") [--strict] [--out <path>]

Capture the host environment (kernel, CPU, memory, KVM/cgroup capabilities)
as JSON. JSON goes to stdout and to the output file; logs go to stderr.

  --strict      exit 1 if /dev/kvm is not writable OR cgroup v2 is missing
  --out <path>  output JSON path (default: \$RESULTS_DIR/env.json)
EOF
}

STRICT=0
OUT="$RESULTS_DIR/env.json"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --strict)
      STRICT=1
      shift
      ;;
    --out)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      OUT="$2"
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

OS="$(uname -s)"
if [ "$OS" != "Linux" ]; then
  log "WARNING: non-Linux host ($OS); env-check is informational only here (/proc-based fields will be null/false)"
fi

timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
kernel="$(uname -r)"

cpu_model=""
if [ -r /proc/cpuinfo ]; then
  cpu_model="$(grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2- \
    | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' || true)"
fi

nproc_val="null"
if command -v nproc >/dev/null 2>&1; then
  nproc_val="$(nproc)"
elif command -v getconf >/dev/null 2>&1; then
  nproc_val="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo null)"
fi
[ -n "$nproc_val" ] || nproc_val="null"

mem_total_kb="null"
if [ -r /proc/meminfo ]; then
  mem_total_kb="$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)"
fi
[ -n "$mem_total_kb" ] || mem_total_kb="null"

disk_free_kb="null"
work_parent="$(dirname "$WORK_DIR")"
if df_out="$(df -Pk "$work_parent" 2>/dev/null)"; then
  disk_free_kb="$(echo "$df_out" | awk 'NR==2 {print $4}')"
fi
[ -n "$disk_free_kb" ] || disk_free_kb="null"

kvm=false
if [ -e /dev/kvm ]; then kvm=true; fi
kvm_writable=false
if [ -w /dev/kvm ]; then kvm_writable=true; fi
cgroup_v2=false
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then cgroup_v2=true; fi

unprivileged_userfaultfd="-1"
if [ -r /proc/sys/vm/unprivileged_userfaultfd ]; then
  unprivileged_userfaultfd="$(cat /proc/sys/vm/unprivileged_userfaultfd)"
fi

virt="unknown"
if command -v systemd-detect-virt >/dev/null 2>&1; then
  detected="$(systemd-detect-virt 2>/dev/null || true)"
  if [ "$detected" = "none" ]; then
    virt="bare-metal"
  elif [ -n "$detected" ]; then
    virt="nested:$detected"
  fi
fi

provider="local"
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then provider="github-actions"; fi

mkdir -p "$RESULTS_DIR" "$(dirname "$OUT")"

if command -v jq >/dev/null 2>&1; then
  json="$(jq -n \
    --arg timestamp "$timestamp" \
    --arg kernel "$kernel" \
    --arg os "$OS" \
    --arg cpu_model "$cpu_model" \
    --arg virt "$virt" \
    --arg provider "$provider" \
    --argjson nproc "$nproc_val" \
    --argjson mem_total_kb "$mem_total_kb" \
    --argjson disk_free_kb "$disk_free_kb" \
    --argjson kvm "$kvm" \
    --argjson kvm_writable "$kvm_writable" \
    --argjson cgroup_v2 "$cgroup_v2" \
    --argjson unprivileged_userfaultfd "$unprivileged_userfaultfd" \
    '{
      timestamp: $timestamp,
      kernel: $kernel,
      os: $os,
      cpu_model: (if $cpu_model == "" then null else $cpu_model end),
      nproc: $nproc,
      mem_total_kb: $mem_total_kb,
      disk_free_kb: $disk_free_kb,
      kvm: $kvm,
      kvm_writable: $kvm_writable,
      cgroup_v2: $cgroup_v2,
      unprivileged_userfaultfd: $unprivileged_userfaultfd,
      virt: $virt,
      provider: $provider
    }')"
else
  json_escape() {
    local s="$1"
    s="${s//\\/\\\\}"
    s="${s//\"/\\\"}"
    printf '%s' "$s"
  }
  json_str_or_null() {
    if [ -z "$1" ]; then printf 'null'; else printf '"%s"' "$(json_escape "$1")"; fi
  }
  json="$(printf '{"timestamp":"%s","kernel":"%s","os":"%s","cpu_model":%s,"nproc":%s,"mem_total_kb":%s,"disk_free_kb":%s,"kvm":%s,"kvm_writable":%s,"cgroup_v2":%s,"unprivileged_userfaultfd":%s,"virt":"%s","provider":"%s"}' \
    "$(json_escape "$timestamp")" \
    "$(json_escape "$kernel")" \
    "$(json_escape "$OS")" \
    "$(json_str_or_null "$cpu_model")" \
    "$nproc_val" \
    "$mem_total_kb" \
    "$disk_free_kb" \
    "$kvm" \
    "$kvm_writable" \
    "$cgroup_v2" \
    "$unprivileged_userfaultfd" \
    "$(json_escape "$virt")" \
    "$(json_escape "$provider")")"
fi

printf '%s\n' "$json" > "$OUT"
printf '%s\n' "$json"
log "wrote $OUT"

{
  echo "---- environment summary ----"
  printf '%-26s %s\n' "timestamp" "$timestamp"
  printf '%-26s %s\n' "os / kernel" "$OS / $kernel"
  printf '%-26s %s\n' "cpu_model" "${cpu_model:-<unknown>}"
  printf '%-26s %s\n' "nproc" "$nproc_val"
  printf '%-26s %s\n' "mem_total_kb" "$mem_total_kb"
  printf '%-26s %s\n' "disk_free_kb" "$disk_free_kb"
  printf '%-26s %s\n' "kvm / writable" "$kvm / $kvm_writable"
  printf '%-26s %s\n' "cgroup_v2" "$cgroup_v2"
  printf '%-26s %s\n' "unprivileged_userfaultfd" "$unprivileged_userfaultfd"
  printf '%-26s %s\n' "virt" "$virt"
  printf '%-26s %s\n' "provider" "$provider"
  echo "-----------------------------"
} >&2

if [ "$STRICT" -eq 1 ]; then
  strict_fail=0
  if [ "$kvm_writable" != "true" ]; then
    log "STRICT: /dev/kvm is not writable"
    strict_fail=1
  fi
  if [ "$cgroup_v2" != "true" ]; then
    log "STRICT: cgroup v2 is missing (/sys/fs/cgroup/cgroup.controllers)"
    strict_fail=1
  fi
  if [ "$strict_fail" -ne 0 ]; then
    exit 1
  fi
fi
