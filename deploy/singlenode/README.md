# Single-node deployment (placeholder)

Single-node, one-command deployment — `embervm dev`, a single process bundling
API server + scheduler + node agent — **lands in M1**. This directory will
then hold the install script (`install.sh`) and single-node configuration.

## What you can run today (M0 proof of concept)

Until then, the M0 POC lives in [`scripts/`](../../scripts/) and is meant to be
run on one Linux host with `/dev/kvm`:

| Script | Purpose |
|---|---|
| `env-check.sh` | Verify host prerequisites (KVM, kernel features, tools) |
| `fetch-assets.sh` | Download Firecracker binaries + guest kernel/rootfs test assets |
| `build-rootfs.sh` | Build the writable guest root filesystem |
| `fc-boot.sh` | Boot a Firecracker microVM |
| `fc-snapshot.sh` | Pause a running microVM and take a snapshot |
| `fc-restore.sh` | Restore a microVM from a snapshot (timed) |

The M0 baseline benchmark suite (restore-latency / throughput matrix) is in
[`test/bench/`](../../test/bench/).

## Where to run it

You need bare metal or nested virtualization. See the verified cloud-server
matrix — Hetzner, GCP, AWS, Azure, DigitalOcean, GitHub Actions — in
[docs/zh/06-云服务器实测指南.md](../../docs/zh/06-云服务器实测指南.md)
(summary table in the top-level [README](../../README.md)).
