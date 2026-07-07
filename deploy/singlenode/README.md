# Single-node deployment (`embervm dev`)

`embervm dev` runs the whole EmberVM stack — REST API, scheduler, and node
agent — in **one root process** on a single Linux host with `/dev/kvm`. It is
the fastest way to try EmberVM end to end (docs/zh/05 §6).

## Prerequisites

- Ubuntu 24.04 (or similar) on **bare metal or a nested-virtualization cloud
  VM** with a writable `/dev/kvm` (see the verified matrix in
  [docs/zh/06](../../docs/zh/06-云服务器实测指南.md)).
- Root access.

## One-command install

```bash
sudo bash deploy/singlenode/install.sh
```

This installs PostgreSQL + ZFS userland + e2fsprogs, creates the `embervm`
database, provisions a **loopback-backed** ZFS pool under `/var/lib/embervm`
(trial-safe — it never touches a real disk unless you ask), fetches
Firecracker assets, builds the binaries, and installs a `systemd` unit.

For a real disk-backed pool (production):

```bash
sudo bash deploy/singlenode/install.sh --pool-device /dev/nvme1n1   # DESTRUCTIVE
```

Then:

```bash
sudo systemctl start embervm
```

## Smoke test

The install prints a dev token (`dev-token`). Drive the full lifecycle:

```bash
API=localhost:8080
AUTH='Authorization: Bearer dev-token'

# Build a template from a Docker image (pull → flatten → guestd → ext4).
TID=$(curl -sXPOST $API/v0/templates -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"web","image":"alpine:3.20"}' | jq -r .id)

# Create a sandbox microVM from it.
SID=$(curl -sXPOST $API/v0/sandboxes -H "$AUTH" -H 'Content-Type: application/json' \
  -d "{\"template_id\":\"$TID\",\"vcpus\":1,\"memory_mib\":256,\"data_disk_gib\":15}" | jq -r .id)

# Run a command inside it.
curl -sXPOST $API/v0/sandboxes/$SID/exec -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo","args":["hello from the sandbox"]}'

# Pause (hot snapshot) and resume (uffd).
curl -sXPOST $API/v0/sandboxes/$SID/pause  -H "$AUTH"
curl -sXPOST $API/v0/sandboxes/$SID/resume -H "$AUTH"

# Tear it down.
curl -sXDELETE $API/v0/sandboxes/$SID -H "$AUTH"
```

## Running without systemd

```bash
sudo bash deploy/singlenode/install.sh --no-systemd
# then run the printed `embervm dev ...` command
```

## Notes (M1)

- **Storage**: `--pool-device` gives a real ZFS pool; the default loopback pool
  is for trials only. The data disk is a sparse raw file on the dataset — a
  15 GiB disk costs nothing until written (docs/zh/04 §1).
- **Auth**: with no `--tokens-file`, a single `dev-token` is used (owner `dev`,
  quota 100). Provide a JSON `token → {owner, max_sandboxes}` file for real
  multi-tenant use.
- **Jailer / host hardening** (chroot, per-VM uid/gid, seccomp) is an **M4**
  item (docs/zh/03); M1 isolates sandboxes with per-namespace networking and
  best-effort cgroup v2.
