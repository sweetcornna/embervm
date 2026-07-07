# Security Policy

EmberVM runs untrusted, multi-tenant workloads inside Firecracker microVMs.
**Multi-tenant isolation is this project's security lifeline** — a sandbox
that can escape, read a neighbor's memory or disk, or exhaust the host is a
broken product, not a degraded one. We treat isolation vulnerabilities
accordingly (see [docs/zh/05-开源项目规划.md](docs/zh/05-开源项目规划.md) §5).

## Reporting a vulnerability

**Please do NOT open a public GitHub issue, discussion, or pull request for
security vulnerabilities.** Public reports put every self-hosted deployment at
risk before a fix exists.

Instead, use **GitHub Security Advisories** (private disclosure):

1. Go to the repository's **Security** tab.
2. Click **"Report a vulnerability"** and fill in the private advisory form.

Include, where possible: affected component (e.g. `nodeagent`, `uffd` handler,
guest kernel/rootfs assets, network setup scripts), reproduction steps or a
proof of concept, the deployment mode (single-node dev vs multi-node), and
your assessment of impact.

### Our commitment

- **First response within 48 hours** of a private report — acknowledgment plus
  an initial triage (severity class and next steps).
- We will keep you informed as we investigate, and credit you in the advisory
  and release notes unless you prefer otherwise.
- We coordinate disclosure with the reporter; we ask that you give us a
  reasonable window to ship a fix before publishing details.

## Severity priorities

| Class | Examples | Priority |
|---|---|---|
| **Sandbox escape** | Guest code escaping the microVM, KVM/Firecracker breakout, escaping the jailer/namespace/cgroup confinement | **Highest — drops everything else** |
| Cross-tenant data exposure | Reading another sandbox's memory pages, snapshot chunks, disk data, or network traffic; chunk-store dedup side channels | Critical |
| Host compromise from the control plane | RCE or privilege escalation via API server / node agent (which runs as root) | Critical |
| Snapshot/state integrity | Tampering with or forging snapshots, manifests, or archived tiers | High |
| Denial of service | A tenant exhausting host CPU/memory/disk/network beyond its quota | High |
| Everything else | Hardening gaps, dependency issues, information leaks in logs | Normal triage |

Sandbox-escape class vulnerabilities are the highest priority this project
recognizes: they are fixed and disclosed ahead of any feature or release work.

## Supported versions

EmberVM is **pre-alpha**. There are no supported releases yet.

| Version | Supported |
|---|---|
| `main` branch | Yes — security fixes land here |
| Anything else (forks, tags, snapshots of `main`) | No |

Once versioned releases begin (`v0.x`), this table will be updated with the
supported release lines. Until then, self-hosters should track `main` and the
host-hardening checklist in
[docs/zh/04-创新与最佳实践.md](docs/zh/04-创新与最佳实践.md).

## Scope notes

- Vulnerabilities in **upstream Firecracker, the Linux kernel, or KVM** should
  be reported upstream (Firecracker has its own security policy). If EmberVM's
  configuration makes an upstream issue exploitable, report it to us as well.
- The M0 proof-of-concept scripts under `scripts/` are development tools and
  assume a trusted operator on a dedicated test machine; they are not a
  multi-tenant deployment surface. Reports against them are still welcome but
  triaged as hardening issues.
