# M1 Phase 2: Template Builder (Docker image → ext4 rootfs) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pkg/template` turns any Docker/OCI image reference into a bootable ext4 rootfs with
guestd baked in as PID 1, plus a manifest capturing the image's runtime defaults.

**Architecture:** Two stages with a tar stream in between, so everything after the registry
pull is unit-testable offline: (1) `Flatten` — daemonless pull+flatten via
go-containerregistry's crane (network); (2) `BuildFromTar` — safe tar extraction into a
staging tree, inject guestd + `/etc/embervm/image.json` + resolv.conf/hostname/hosts, ensure
mount-point dirs, then `mkfs.ext4 -d` (linux binary; macOS skips at test level). Reuses the
atomic tmp+rename convention from scripts/build-rootfs.sh.

**Tech Stack:** Go 1.24, github.com/google/go-containerregistry (Apache-2.0, registered in
THIRD_PARTY_NOTICES.md), archive/tar, mkfs.ext4 (e2fsprogs ≥1.47 for unprivileged `-d`).

## Global Constraints

- Daemonless: no Docker daemon anywhere (CI runners have one, production nodes won't).
- Tar extraction must reject path traversal (`..`, absolute link targets escaping the root).
- Non-root friendly: chown/mknod failures are skipped silently when running unprivileged
  (unit tests); production builds run as root under nodeagent and preserve ownership.
- Image runtime defaults (Env/Entrypoint/Cmd/WorkingDir/User) recorded to
  `/etc/embervm/image.json` — guestd exec uses them as defaults in a later iteration; M1 v0
  only records them.
- ext4 size: `--size-mb` override, else `max(2×tree size, tree+512MiB)` rounded up to 64MiB.
- squashfs+overlayfs base sharing is deliberately deferred (master spec D4); ext4 satisfies
  the M1 charter line "Docker 镜像 → ext4/squashfs rootfs".

---

### Task 1: safe tar extractor

**Files:**
- Create: `pkg/template/untar.go`
- Test: `pkg/template/untar_test.go`

**Interfaces (Produces):**
```go
package template
// Untar extracts a flattened filesystem tar into dst. It handles dirs,
// regular files, symlinks and hardlinks; preserves modes; applies uid/gid
// and device nodes only when running as root (EPERM is skipped otherwise).
// Entries escaping dst (via .. or absolute hardlink targets) are an error;
// symlink TARGETS may be absolute (they are guest paths, resolved at guest
// runtime, not extraction time).
func Untar(dst string, r io.Reader) error
```

**Tests (build the tar in-memory with archive/tar):**
1. dir + file (mode 0755/0640) + symlink (`/usr/bin/sh -> /bin/busybox` style absolute target
   allowed) + hardlink → extracted tree matches, modes match, hardlink shares inode.
2. entry name `../escape` → error; hardlink `Linkname: ../../etc/passwd` → error.
3. PAX/long names (255+ chars) roundtrip.
4. re-extract over existing tree (idempotent overwrite, symlink replaced not followed).

- [ ] Step 1: write untar_test.go → `go test ./pkg/template/` FAILs (no package)
- [ ] Step 2: implement untar.go → PASS
- [ ] Step 3: commit `feat(template): safe tar extractor`

### Task 2: staging injection + image manifest

**Files:**
- Create: `pkg/template/inject.go` (guestd install, etc files, mount-point dirs)
- Create: `pkg/template/manifest.go` (image.json types + write)
- Test: `pkg/template/inject_test.go`

**Interfaces (Produces):**
```go
// ImageConfig is the subset of the OCI config guestd may need at runtime.
type ImageConfig struct {
	Image      string   `json:"image"`                 // original reference
	Env        []string `json:"env,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`
}
// injectRuntime installs guestd (guestdPath) at /usr/local/bin/guestd
// (0755), writes /etc/embervm/image.json, /etc/resolv.conf (8.8.8.8),
// /etc/hostname ("ember"), /etc/hosts (localhost + ember), and ensures
// /proc /sys /dev /tmp /run /etc exist.
func injectRuntime(root, guestdPath string, cfg ImageConfig) error
```

**Tests:** run against a staging dir from Task 1's fixture; assert guestd bytes+mode, valid
image.json roundtrip, etc files content, mount-point dirs exist even when absent from tar.

- [ ] Steps: test-first as Task 1; commit `feat(template): rootfs runtime injection`

### Task 3: mkfs.ext4 image assembly (linux-gated)

**Files:**
- Create: `pkg/template/ext4_linux.go` + `pkg/template/ext4_stub.go` (`!linux` → error)
- Test: `pkg/template/ext4_linux_test.go` (skips if mkfs.ext4 or debugfs missing)

**Interfaces (Produces):**
```go
// mkext4 builds img (tmp+rename atomic) from the staging tree. sizeMB 0 →
// max(2×tree, tree+512MiB) rounded up to 64MiB.
func mkext4(img, stagingRoot string, sizeMB int) error
```
Implementation mirrors scripts/build-rootfs.sh steps 6-7: `truncate -s` (or os.Truncate on a
created file) then `mkfs.ext4 -F -q -d root img.tmp` then rename.

**Test (linux only):** build from fixture tree; `debugfs -R "stat /etc/hostname" img` exits 0
(file present) — no mount needed, works unprivileged.

- [ ] Steps: test-first; commit `feat(template): ext4 image assembly`

### Task 4: Builder facade + crane flatten

**Files:**
- Create: `pkg/template/builder.go` (`Build`), `pkg/template/flatten.go` (crane wrapper)
- Modify: `go.mod` (+go-containerregistry), `THIRD_PARTY_NOTICES.md` (+dependency entry)
- Test: `pkg/template/builder_test.go` (offline: Build with `TarSource`); registry path
  gated behind `EMBERVM_NET_TESTS=1` (`builder_net_test.go`, pulls alpine:3.20)

**Interfaces (Produces — Phase 4's nodeagent BuildTemplate consumes this):**
```go
type BuildInput struct {
	Image      string    // registry reference; mutually exclusive with TarSource
	TarSource  io.Reader // pre-flattened fs tar (tests, air-gapped)
	GuestdPath string    // host path to the static guestd binary
	OutPath    string    // destination rootfs.ext4
	SizeMB     int       // 0 → auto
}
type BuildResult struct {
	Config      ImageConfig
	RootfsBytes int64
}
func Build(ctx context.Context, in BuildInput) (*BuildResult, error)
```
`flatten.go`: `crane.Pull(ref)` → `img.ConfigFile()` for ImageConfig + `mutate.Extract(img)`
as the tar stream (linux/amd64 platform option).

- [ ] Steps: test-first offline path; `go mod tidy`; commit `feat(template): image build pipeline`

### Task 5: CI coverage

**Files:**
- Modify: `.github/workflows/lint-unit.yml` — add step (linux, no KVM needed):
  `EMBERVM_NET_TESTS=1 go test ./pkg/template/ -run TestBuildFromRegistry -v`
  after the unit-test step (mkfs.ext4 + network available on ubuntu-latest).

- [ ] Step 1: edit workflow; `make lint && make test`
- [ ] Step 2: commit `ci: exercise template build from registry in lint-unit`; push
- [ ] Step 3: watch lint-unit + integration-kvm green; mark task #12 complete

## Verification
- Offline unit tests green on macOS + linux (`make test`).
- lint-unit CI job builds a real alpine:3.20 template (pull → flatten → inject → mkfs) and
  debugfs-verifies /usr/local/bin/guestd inside the image.
- Boot-in-microVM validation intentionally deferred to Phase 4 (nodeagent create path),
  where the first KVM e2e boots this exact artifact with init=/usr/local/bin/guestd.
