# Third-Party Notices

This file registers every piece of third-party software, code, content, or
design reference that EmberVM uses, per the code-reuse compliance policy in
[docs/zh/05-开源项目规划.md](docs/zh/05-开源项目规划.md) §2 and
[CONTRIBUTING.md](CONTRIBUTING.md). EmberVM itself is licensed under
**AGPL-3.0** (see [LICENSE](LICENSE)).

## Registry

| # | Component | Upstream | License | How it is used | Redistributed by EmberVM? |
|---|---|---|---|---|---|
| 1 | Firecracker | [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker) | Apache-2.0 | Prebuilt release binaries downloaded at build/test time; uffd example handler used as a design reference | No |
| 2 | Firecracker CI guest assets: `vmlinux` 6.1 kernel | [firecracker-microvm CI artifacts](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md) | GPL-2.0 (Linux kernel) | Downloaded as a test fixture (guest kernel) for local/CI microVM tests only | No |
| 3 | Firecracker CI guest assets: Ubuntu 24.04 squashfs rootfs | firecracker-microvm CI artifacts | Various (Ubuntu component licenses) | Downloaded as a test fixture (guest root filesystem) for local/CI microVM tests only | No |
| 4 | golang.org/x/sys | [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | BSD-3-Clause | Go module dependency (syscall wrappers: userfaultfd ioctls, etc.); statically linked into EmberVM binaries | Yes (in compiled binaries) |
| 5 | Contributor Covenant v2.1 | [contributor-covenant.org](https://www.contributor-covenant.org/version/2/1/code_of_conduct.html) | CC BY 4.0 | Adapted as [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), with attribution | Yes (in this repository) |
| 6 | google/go-containerregistry | [google/go-containerregistry](https://github.com/google/go-containerregistry) | Apache-2.0 | Go module dependency; daemonless pull + flatten of Docker/OCI images in `pkg/template` (the template builder); statically linked into EmberVM binaries | Yes (in compiled binaries) |
| 7 | cyphar/filepath-securejoin | [cyphar/filepath-securejoin](https://github.com/cyphar/filepath-securejoin) | BSD-3-Clause | Go module dependency; scoped symlink resolution that keeps tar extraction (`pkg/template`) inside the destination root (tar-slip defense); statically linked into EmberVM binaries | Yes (in compiled binaries) |

## Entry details

### 1. Firecracker (Apache-2.0)

- EmberVM launches Firecracker as an external process. **Prebuilt Firecracker
  binaries are downloaded from upstream GitHub Releases at build/test time**
  (see `scripts/fetch-assets.sh`) and are **not redistributed** in this
  repository or in EmberVM release artifacts. Users obtain Firecracker under
  its own Apache-2.0 license directly from upstream.
- The **userfaultfd handshake protocol and handler semantics implemented in
  `pkg/uffd` are an independent Go implementation**, informed by studying
  Firecracker's example uffd handler under `src/firecracker/examples/uffd/`
  in the upstream repository (Apache-2.0, design reference). The wire protocol
  (Unix socket handshake, JSON mappings message, uffd file-descriptor passing)
  is defined by Firecracker and must be interoperated with; no Rust code was
  copied or translated. Attribution is retained here and in the `pkg/uffd`
  package documentation.

### 2–3. Firecracker CI guest assets (kernel + rootfs)

- The `vmlinux` 6.1 guest kernel image (**GPL-2.0**, Linux kernel) and the
  Ubuntu 24.04 squashfs root filesystem (**various licenses** — the standard
  Ubuntu component licenses) are published by the Firecracker project as CI
  artifacts for its getting-started flow.
- EmberVM downloads them **as test fixtures only** (`scripts/fetch-assets.sh`)
  to boot real microVMs in local tests and CI. They are **never committed to
  this repository and never redistributed** in EmberVM release artifacts.
  GPL-2.0 obligations therefore do not attach to EmberVM's distribution; the
  guest kernel runs inside user-operated VMs as a separate program.

### 4. golang.org/x/sys (BSD-3-Clause)

- Standard Go extended library, the project's only third-party Go dependency.
  Its BSD-3-Clause license and copyright notice ship with the module source
  and apply to the statically linked code inside released EmberVM binaries.
  BSD-3-Clause is compatible with AGPL-3.0.

### 5. Contributor Covenant v2.1 (CC BY 4.0)

- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) is adapted from the Contributor
  Covenant, version 2.1, by Coraline Ada Ehmke and contributors, licensed
  CC BY 4.0. Attribution is included in the document itself as required.

### 6. google/go-containerregistry (Apache-2.0)

- The template builder (`pkg/template`) uses go-containerregistry's `crane`
  and `mutate` packages to pull a Docker/OCI image and flatten its layers
  into a single filesystem tar **without a Docker daemon** — production
  EmberVM nodes do not run one. Apache-2.0 is one-way compatible with
  AGPL-3.0; the library is a normal Go dependency, statically linked, with
  its license and copyright shipped in the module source.

### 7. cyphar/filepath-securejoin (BSD-3-Clause)

- `pkg/template`'s tar extractor resolves every on-disk write path with
  `securejoin.SecureJoin`, which follows symlinks already present in the
  staging tree but clamps any component that would escape the destination
  back inside it. This is the tar-slip / Zip-Slip defense: a hostile image
  layer cannot place a `foo -> /etc` symlink and then write through it to the
  host. The library is the same audited implementation used by runc and
  containerd. BSD-3-Clause is compatible with AGPL-3.0.

## How to add an entry

Per docs/zh/05 §2, **every** introduction of third-party code or content must
be registered here **in the same pull request** that introduces it:

1. **Check license compatibility first.**
   - Apache-2.0, BSD, MIT, and similarly permissive licenses **may** be
     incorporated into this AGPL-3.0 project (one-way compatible).
   - **BSL-licensed code (e.g. parts of e2b-dev/infra) must NEVER be copied
     or translated** — it is a read-only design reference only. If a
     "reference" starts to look like a port, stop and re-implement from the
     ideas, not the code.
   - GPL-family code: ask maintainers before importing (compatibility depends
     on version and direction).
2. **Preserve upstream notices.** Keep original copyright headers and any
   NOTICE file text in the imported files.
3. **Add a row to the Registry table** above (component, upstream link,
   license, how used, whether redistributed) and, for anything non-trivial, a
   short "Entry details" subsection: what exactly was taken (code / binaries /
   design only), where it lives in this repo, and whether it ships in release
   artifacts.
4. **Distinguish the three usage classes** clearly, since obligations differ:
   - *incorporated code* (ships in the repo/binaries — full attribution),
   - *downloaded artifacts* (fetched at build/test time, not redistributed),
   - *design reference* (no code taken — recorded for provenance transparency).
5. If unsure, open an issue before the import. See
   [CONTRIBUTING.md](CONTRIBUTING.md) for the full compliance red lines.
