//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/fcclient"
	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/metrics"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/template"
)

// guestMAC is the fixed guest NIC address (matches scripts/fc-boot.sh).
const guestMAC = "06:00:AC:10:00:02"

// baseBootArgs configures the guest to bring up eth0 at 172.16.0.2 and boot
// guestd as PID 1; docs/zh/04 §5 microVM args are appended from Config.
const baseBootArgs = "console=ttyS0 reboot=k panic=1 pci=off " +
	"ip=172.16.0.2::172.16.0.1:255.255.255.252:ember:eth0:off " +
	"init=/usr/local/bin/guestd"

// defaultExtraArgs is docs/zh/04 §5's microVM kernel command line.
const defaultExtraArgs = "8250.nr_uarts=0 swiotlb=noforce"

// hotplugSlotMiB is Firecracker's KVM slot granularity for the virtio-mem
// region: total_size_mib must be a multiple of it (docs/memory-hotplug.md).
const hotplugSlotMiB = 128

// hotplugBootArgs makes the guest online hotplugged blocks automatically and
// into ZONE_MOVABLE, so hot-UNplug can migrate pages away and actually hand
// memory back to the host. The memmap_on_memory alternative avoids the ~1.6%
// boot-memory memmap cost but makes unplug unreliable — reclaim is the whole
// point of the M6 oversell story, so movable wins (ADR-0007).
const hotplugBootArgs = "memhp_default_state=online_movable"

// roundUpMiB rounds n up to the next multiple of q.
func roundUpMiB(n, q int) int {
	return (n + q - 1) / q * q
}

// hotplugTotalMiB is the size of sb's virtio-mem region (0 = none).
func (sb *sandbox) hotplugTotalMiB() int {
	return sb.maxMemMiB - sb.baseMemMiB
}

type sandbox struct {
	id          string
	machine     *lifecycle.Machine
	lease       netns.Lease
	dir         string
	vcpus       int
	memMiB      int // current effective guest memory (base + plugged hotplug)
	baseMemMiB  int // boot mem_size_mib; fixed for the VM's lifetime (M6)
	maxMemMiB   int // hotplug ceiling; == baseMemMiB when no hotplug region (M6)
	maxVCPUs    int // boot-time core count; == vcpus when CPU resize is off (M6)
	rootfs      string
	dataRaw     string
	fc          *exec.Cmd
	uffd        *exec.Cmd
	snapMu      sync.Mutex // serializes layer-file mutation (Full-pause reset, rollback trim) with fork's chain read
	snapCount   int
	guest       *guestapi.Client
	templateID  string
	dataDiskGiB int
	mountDir    string              // dataset mountpoint (drive paths live here)
	layers      []*memsnap.Manifest // chunked memory chain, full root first
	diskLayers  []string            // zfs delta chain tags (outlives memory-chain restarts)
	snapLayer   string              // layer whose snapfile the next resume loads ("p3", "cold", ...)
	restoreTier string              // tier the last restore pulled from ("" = local)
	diskOrigin  *DiskOrigin         // non-nil when the disk chain roots off another sandbox (golden clone)
	egress      string              // "nat" (default) | "none"
	// forceFullPause roots a fresh Full chain on the next pause (set after
	// a cold restore: the synthetic-full parent lives in the cold store).
	forceFullPause bool
}

// Agent is the concrete linux node agent.
type Agent struct {
	cfg        Config
	mu         sync.Mutex
	sbx        map[string]*sandbox
	localStore *chunkstore.Dir    // node-local chunk cache (chunked mode)
	l1         chunkstore.Backend // optional L1 object store (EMBERVM_L1_*)
	cold       chunkstore.Backend // optional cold-tier store (EMBERVM_COLD_*)
	failed     []string           // watchdog-reaped ids, drained by Healthz
	golden     map[string]goldenMeta

	watchdogCancel context.CancelFunc // set when New started the watchdog
}

// Close stops the agent's background watchdog goroutine. Sandboxes are left
// as they are — Close is for tests and orderly daemon shutdown, not teardown.
func (a *Agent) Close() {
	if a.watchdogCancel != nil {
		a.watchdogCancel()
	}
}

var _ nodeapi.Agent = (*Agent)(nil)

// New constructs a node agent. It fills defaults but does not create the
// netns pool (call Config.Pool.Setup separately at daemon start).
func New(cfg Config) (nodeapi.Agent, error) {
	if cfg.Storage == nil || cfg.Pool == nil {
		return nil, fmt.Errorf("nodeagent: Storage and Pool are required")
	}
	if cfg.WorkDir == "" || cfg.KernelPath == "" || cfg.FCBin == "" || cfg.GuestdBin == "" {
		return nil, fmt.Errorf("nodeagent: WorkDir, KernelPath, FCBin, GuestdBin are required")
	}
	if cfg.RestoreMode == "" {
		cfg.RestoreMode = "prefetch"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup/embervm"
	}
	if cfg.BootExtraArgs == "" {
		cfg.BootExtraArgs = defaultExtraArgs
	}
	if cfg.JailerChrootBase == "" {
		cfg.JailerChrootBase = "/srv/jailer"
	}
	if cfg.JailerBin != "" && cfg.RestoreMode != "chunked" {
		// The M1 raw-memfile paths predate chroot-relative path handling;
		// hardened deployments use the M2+ pipeline.
		return nil, fmt.Errorf("nodeagent: jailer requires restore_mode=chunked")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, sbx: map[string]*sandbox{}, golden: map[string]goldenMeta{}}
	if a.chunked() {
		if cfg.ChunkStoreDir == "" {
			cfg.ChunkStoreDir = filepath.Join(cfg.WorkDir, "chunks")
			a.cfg.ChunkStoreDir = cfg.ChunkStoreDir
		}
		local, err := chunkstore.NewDir(cfg.ChunkStoreDir)
		if err != nil {
			return nil, err
		}
		a.localStore = local
		l1, ok, err := chunkstore.L1FromEnv()
		if err != nil {
			return nil, err
		}
		if ok {
			a.l1 = l1
		}
		cold, ok, err := chunkstore.ColdFromEnv()
		if err != nil {
			return nil, err
		}
		if ok {
			a.cold = cold
		}
	}
	if cfg.WatchdogInterval > 0 {
		// Runs for the daemon's lifetime unless Close is called.
		wctx, cancel := context.WithCancel(context.Background())
		a.watchdogCancel = cancel
		a.StartWatchdog(wctx, cfg.WatchdogInterval)
	}
	return a, nil
}

// BuildTemplate builds a rootfs from image and imports it as a template.
func (a *Agent) BuildTemplate(ctx context.Context, templateID, image string) error {
	out := filepath.Join(a.cfg.WorkDir, "build-"+templateID+".ext4")
	defer os.Remove(out)
	if _, err := template.Build(ctx, template.BuildInput{
		Image:      image,
		GuestdPath: a.cfg.GuestdBin,
		OutPath:    out,
	}); err != nil {
		return fmt.Errorf("build template %s: %w", templateID, err)
	}
	if err := a.cfg.Storage.EnsureTemplate(ctx, templateID, out); err != nil {
		return err
	}
	if a.chunked() {
		if err := a.pushTemplateL1(ctx, templateID); err != nil {
			return fmt.Errorf("push template %s to L1: %w", templateID, err)
		}
	}
	if a.goldenEnabled() {
		if err := a.buildGolden(ctx, templateID); err != nil {
			// Fast-create is an optimization; the template itself is fine.
			log.Printf("nodeagent: golden snapshot for %s failed (cold boots continue to work): %v", templateID, err)
		}
	}
	return nil
}

// CreateSandbox clones storage, boots a microVM, and waits for guestd.
func (a *Agent) CreateSandbox(ctx context.Context, req nodeapi.CreateSandboxRequest) (nodeapi.SandboxStatus, error) {
	createStart := time.Now()
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	if req.MemoryMiB == 0 {
		req.MemoryMiB = 256
	}
	if req.DataDiskGiB == 0 {
		req.DataDiskGiB = 15
	}
	// Normalize the resize ceilings once, here, so every downstream consumer
	// (boot, cgroup, descriptor, resize verb) sees the same values: the
	// hotplug region must be a multiple of the 128 MiB KVM slot, so the
	// effective memory ceiling is base + rounded-up region.
	if req.MaxMemoryMiB > req.MemoryMiB {
		req.MaxMemoryMiB = req.MemoryMiB + roundUpMiB(req.MaxMemoryMiB-req.MemoryMiB, hotplugSlotMiB)
	} else {
		req.MaxMemoryMiB = req.MemoryMiB
	}
	if req.MaxVCPUs < req.VCPUs {
		req.MaxVCPUs = req.VCPUs
	}

	if meta, ok := a.goldenFor(ctx, req.TemplateID, req); ok {
		st, err := a.fastCreate(ctx, req, meta)
		if err == nil {
			metrics.CreateSeconds.WithLabelValues("fast").Observe(time.Since(createStart).Seconds())
			return st, nil
		}
		if ctx.Err() != nil {
			return nodeapi.SandboxStatus{}, err
		}
		// Fast-create is an optimization: a runtime failure (a missing
		// golden chunk, a clone error) must not fail the create — fall
		// back to the cold boot below. cloneRestore already dismantled
		// its partial sandbox.
		log.Printf("nodeagent: fast create %s: %v; falling back to cold boot", req.SandboxID, err)
	}

	m := lifecycle.New(lifecycle.StatePending)
	_ = m.To(lifecycle.StateStarting)

	// The scheduler may place a create on a node that never built the
	// template — pull the published stream from L1 first.
	if err := a.ensureTemplate(ctx, req.TemplateID); err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("receive template %s: %w", req.TemplateID, err)
	}
	paths, err := a.cfg.Storage.CloneSandbox(ctx, req.SandboxID, req.TemplateID, req.DataDiskGiB)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	lease, err := a.cfg.Pool.Acquire()
	if err != nil {
		_ = a.cfg.Storage.DestroySandbox(ctx, req.SandboxID)
		return nodeapi.SandboxStatus{}, err
	}

	sb := &sandbox{
		id:          req.SandboxID,
		machine:     m,
		lease:       lease,
		dir:         filepath.Join(a.cfg.WorkDir, req.SandboxID),
		vcpus:       req.VCPUs,
		memMiB:      req.MemoryMiB,
		baseMemMiB:  req.MemoryMiB,
		maxMemMiB:   req.MaxMemoryMiB,
		maxVCPUs:    req.MaxVCPUs,
		rootfs:      paths.RootfsExt4,
		dataRaw:     paths.DataRaw,
		templateID:  req.TemplateID,
		dataDiskGiB: req.DataDiskGiB,
		mountDir:    paths.Dir,
		egress:      req.Egress,
	}
	if err := os.MkdirAll(sb.dir, 0o755); err != nil {
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, err
	}
	// Egress before boot: a "none" sandbox must never see the world.
	if err := a.applyEgress(ctx, sb); err != nil {
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, fmt.Errorf("apply egress policy: %w", err)
	}
	if err := a.bootFresh(ctx, sb); err != nil {
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, err
	}
	_ = m.To(lifecycle.StateRunning)

	a.mu.Lock()
	a.sbx[req.SandboxID] = sb
	a.mu.Unlock()

	if err := a.waitGuest(ctx, sb, 30*time.Second); err != nil {
		// A booted-but-unreachable guest is not a sandbox: off the books
		// first (a.get must stop resolving it), then release everything —
		// otherwise the VM, lease, cgroup, and dataset leak as RUNNING and
		// count against capacity forever.
		a.mu.Lock()
		delete(a.sbx, req.SandboxID)
		a.mu.Unlock()
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, fmt.Errorf("guestd did not come up: %w", err)
	}
	metrics.CreateSeconds.WithLabelValues("cold").Observe(time.Since(createStart).Seconds())
	return a.status(sb), nil
}

// launchFC starts the Firecracker process — jailed (chroot + uid/gid +
// netns + seccomp) when a jailer is configured, plain otherwise — and
// returns the API socket path.
func (a *Agent) launchFC(ctx context.Context, sb *sandbox, logName string) (string, error) {
	apiSock := a.fcAPISock(sb)
	// sun_path is 108 bytes on Linux. A too-long host-side path fails every
	// connect(2) with EINVAL while Firecracker sits happily listening on the
	// short in-chroot path — a silent 10s timeout instead of an answer. The
	// jailed path grows with the chroot base AND the sandbox id (uuid), so
	// validate loudly up front.
	if len(apiSock) > 104 {
		return "", fmt.Errorf("api socket path is %d bytes (unix sun_path limit): %s — use a shorter jailer chroot base or work dir", len(apiSock), apiSock)
	}
	_ = os.Remove(apiSock)
	var fc *exec.Cmd
	if a.jailed() {
		if err := a.buildJail(sb); err != nil {
			return "", fmt.Errorf("build jail: %w", err)
		}
		fc = a.jailerCommand(sb)
	} else {
		fc = exec.Command("ip", "netns", "exec", sb.lease.Netns,
			a.cfg.FCBin, "--api-sock", apiSock)
	}
	logf, _ := os.Create(filepath.Join(sb.dir, logName))
	if logf != nil {
		fc.Stdout, fc.Stderr = logf, logf
		// Start dups the fd into the child; the parent copy is a pure leak.
		defer logf.Close()
	}
	if err := fc.Start(); err != nil {
		return "", fmt.Errorf("start firecracker: %w", err)
	}
	a.mu.Lock()
	sb.fc = fc
	a.mu.Unlock()
	// The cgroup memory ceiling covers the declared resize maximum, not the
	// current effective size: hotplug growth must not trip memory.max
	// mid-resize; the max is the hard bound against a hostile guest. The CPU
	// quota clamps to the CURRENT effective vcpus — only when the VM booted
	// with headroom cores (maxVCPUs > vcpus is meaningless without it).
	quota := 0
	if sb.maxVCPUs > sb.vcpus {
		quota = sb.vcpus
	}
	a.placeCgroup(sb.id, fc.Process.Pid, max(sb.memMiB, sb.maxMemMiB), quota)
	if err := waitSocketReady(ctx, apiSock, 10*time.Second); err != nil {
		return "", err
	}
	return apiSock, nil
}

// fcKernelPath is the kernel path as Firecracker sees it.
func (a *Agent) fcKernelPath() string {
	if a.jailed() {
		return "/vmlinux"
	}
	return a.cfg.KernelPath
}

// bootFresh starts a Firecracker process in the sandbox netns and drives the
// full boot API sequence.
func (a *Agent) bootFresh(ctx context.Context, sb *sandbox) error {
	apiSock, err := a.launchFC(ctx, sb, "fc.log")
	if err != nil {
		return err
	}
	c := fcclient.New(apiSock)
	bootArgs := baseBootArgs + " " + a.cfg.BootExtraArgs
	if sb.hotplugTotalMiB() > 0 {
		// Hotplugged blocks must land in ZONE_MOVABLE or unplug can never
		// reclaim them (M6).
		bootArgs += " " + hotplugBootArgs
	}
	steps := []func() error{
		func() error {
			// maxVCPUs cores at boot (Firecracker cannot hotplug them);
			// launchFC's cpu.max quota clamps effective compute to sb.vcpus.
			return c.PutMachineConfig(ctx, fcclient.MachineConfig{
				VCPUCount: max(sb.maxVCPUs, sb.vcpus), MemSizeMiB: sb.baseMemMiB,
				TrackDirtyPages: a.chunked(), // Diff snapshots need dirty logging
			})
		},
		func() error {
			return c.PutBootSource(ctx, fcclient.BootSource{KernelImagePath: a.fcKernelPath(), BootArgs: bootArgs})
		},
		func() error {
			return c.PutDrive(ctx, fcclient.Drive{DriveID: "rootfs", PathOnHost: a.fcDrivePath(sb, sb.rootfs), IsRootDevice: true})
		},
		func() error {
			return c.PutDrive(ctx, fcclient.Drive{DriveID: "data", PathOnHost: a.fcDrivePath(sb, sb.dataRaw)})
		},
		func() error {
			// Balloon device: 0 = nothing reclaimed until SetBalloon asks
			// (M4 memory oversell); survives snapshots.
			return c.PutBalloon(ctx, fcclient.Balloon{AmountMib: 0, DeflateOnOom: true})
		},
		func() error {
			// virtio-mem region for runtime memory resize (M6). Pre-boot
			// only; starts fully unplugged, so it costs nothing until a
			// resize plugs blocks. Snapshot-restored VMs inherit the region
			// from the snapshot instead.
			if sb.hotplugTotalMiB() == 0 {
				return nil
			}
			return c.PutMemoryHotplug(ctx, fcclient.MemoryHotplug{TotalSizeMiB: sb.hotplugTotalMiB()})
		},
		func() error {
			return c.PutNetworkInterface(ctx, fcclient.NetworkInterface{IfaceID: "eth0", GuestMAC: guestMAC, HostDevName: "tap0"})
		},
		func() error { return c.InstanceStart(ctx) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	sb.guest = a.guestClient(sb)
	return nil
}

// PauseSandbox snapshots the VM (Full) and kills the process.
func (a *Agent) PauseSandbox(ctx context.Context, sandboxID string) error {
	sb, err := a.get(sandboxID)
	if err != nil {
		return err
	}
	if err := sb.machine.To(lifecycle.StatePausing); err != nil {
		return err
	}
	if err := a.doPause(ctx, sb); err != nil {
		a.abortPause(ctx, sb, err)
		return err
	}
	return sb.machine.To(lifecycle.StatePausedHot)
}

// doPause runs the mode-specific pause pipeline. The machine is already in
// PAUSING; the caller owns the rollback on error (abortPause).
func (a *Agent) doPause(ctx context.Context, sb *sandbox) error {
	c := fcclient.New(a.fcAPISock(sb))
	if a.chunked() && a.cfg.PauseBalloonSettle > 0 {
		// Balloon-assisted pause (docs/zh/02 §3): inflating hands the
		// guest's free pages back before the snapshot, and the chunk
		// pipeline's zero-page skip drops them from the diff (CodeSandbox:
		// 16GiB 快照 13GiB 可跳过). Best-effort — a guest kernel without
		// virtio-balloon simply frees nothing.
		if err := c.PatchBalloon(ctx, sb.memMiB/2); err != nil {
			log.Printf("nodeagent: balloon inflate %s: %v", sb.id, err)
		} else {
			time.Sleep(a.cfg.PauseBalloonSettle)
		}
	}
	if err := c.PatchVMState(ctx, "Paused"); err != nil {
		return err
	}
	if a.chunked() {
		return a.pauseChunked(ctx, sb)
	}
	snapDir := filepath.Join(sb.dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	if err := c.CreateSnapshot(ctx, fcclient.SnapshotCreate{
		SnapshotPath: filepath.Join(snapDir, "snapfile"),
		MemFilePath:  filepath.Join(snapDir, "memfile"),
	}); err != nil {
		return err
	}
	a.killFC(sb)
	sb.snapCount++
	if _, err := a.cfg.Storage.Snapshot(ctx, sb.id, "p"+strconv.Itoa(sb.snapCount)); err != nil {
		return err
	}
	return nil
}

// abortPause unwedges a failed pause: PAUSING's only forward edge is
// →PAUSED_HOT, which a failed pause never reaches, so without a rollback
// the sandbox is stranded forever. With Firecracker still alive the
// snapshot never completed — resume the VM in place and go back to
// RUNNING. Past the kill, land in FAILED: the last completed write-through
// snapshot restores it (crash semantics), locally or elsewhere.
func (a *Agent) abortPause(ctx context.Context, sb *sandbox, cause error) {
	a.mu.Lock()
	alive := sb.fc != nil
	a.mu.Unlock()
	if alive {
		if err := fcclient.New(a.fcAPISock(sb)).PatchVMState(ctx, "Resumed"); err == nil {
			if sb.machine.CAS(lifecycle.StatePausing, lifecycle.StateRunning) == nil {
				log.Printf("nodeagent: pause %s aborted (%v); VM resumed in place", sb.id, cause)
				return
			}
		}
	}
	a.killFC(sb)
	a.killUffd(sb)
	_ = sb.machine.CAS(lifecycle.StatePausing, lifecycle.StateFailed)
	log.Printf("nodeagent: pause %s failed (%v); sandbox FAILED, restorable from its last completed snapshot", sb.id, cause)
}

// ResumeSandbox restores the VM from its snapshot via the uffd handler.
func (a *Agent) ResumeSandbox(ctx context.Context, sandboxID string) (nodeapi.SandboxStatus, error) {
	start := time.Now()
	st, err := a.resume(ctx, sandboxID)
	if err == nil {
		metrics.RestoreSeconds.WithLabelValues("hot").Observe(time.Since(start).Seconds())
	}
	return st, err
}

// resume is the shared implementation. RestoreSandbox, fastCreate, and
// SnapshotSandbox come here directly: the flow the user invoked owns the
// metrics observation, so nothing is counted twice.
func (a *Agent) resume(ctx context.Context, sandboxID string) (st nodeapi.SandboxStatus, err error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if err := sb.machine.To(lifecycle.StateResuming); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	// A failed resume must not strand a half-started sandbox: RESUMING's only
	// forward edge is →RUNNING, so without this rollback the machine wedges
	// forever while the just-launched uffd handler and Firecracker leak. Kill
	// whatever was started and land in FAILED — recoverable via the restore
	// path (the watchdog's reapZombies comment relies on exactly this).
	defer func() {
		if err == nil {
			return
		}
		a.killFC(sb)
		a.killUffd(sb)
		if cerr := sb.machine.CAS(lifecycle.StateResuming, lifecycle.StateFailed); cerr != nil {
			// Already past To(StateRunning): a waitGuest failure leaves a
			// booted-but-unreachable guest — equally FAILED.
			_ = sb.machine.CAS(lifecycle.StateRunning, lifecycle.StateFailed)
		}
	}()
	snapDir := filepath.Join(sb.dir, "snap")
	uffdSock := filepath.Join(snapDir, "uffd.sock")
	_ = os.Remove(uffdSock)

	// Start the memory handler (listens on uffdSock) before FC connects.
	var uffd *exec.Cmd
	if a.chunked() {
		uffd = exec.Command(a.cfg.UffdHandlerBin,
			"--socket", uffdSock,
			"--mode", "chunked",
			"--manifest-dir", snapDir,
			"--store", a.cfg.ChunkStoreDir,
			"--ws", sb.wsPath(),
			"--parent-pid", strconv.Itoa(os.Getpid()))
		// A cold-tier restore serves faults from the cold store: re-point
		// the handler's L1 fallback there (nil env = inherit for warm/hot).
		uffd.Env = handlerEnvForTier(sb.restoreTier)
	} else {
		uffd = exec.Command(a.cfg.UffdHandlerBin,
			"--socket", uffdSock,
			"--memfile", filepath.Join(snapDir, "memfile"),
			"--mode", a.cfg.RestoreMode)
	}
	ulog, _ := os.Create(filepath.Join(sb.dir, "uffd.log"))
	if ulog != nil {
		uffd.Stdout, uffd.Stderr = ulog, ulog
		defer ulog.Close()
	}
	if err := uffd.Start(); err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("start uffd handler: %w", err)
	}
	a.mu.Lock()
	sb.uffd = uffd
	a.mu.Unlock()
	if err := waitSocket(ctx, uffdSock, 5*time.Second); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	// The handler runs as root; a jailed Firecracker connects as its own
	// uid and needs write permission on the socket inode.
	if a.jailed() {
		if err := os.Chmod(uffdSock, 0o666); err != nil {
			return nodeapi.SandboxStatus{}, err
		}
	}

	apiSock, err := a.launchFC(ctx, sb, "fc-resume.log")
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("start firecracker (resume): %w", err)
	}

	c := fcclient.New(apiSock)
	load := fcclient.SnapshotLoad{
		SnapshotPath: filepath.Join(snapDir, "snapfile"),
		MemBackend:   fcclient.MemBackend{BackendType: "Uffd", BackendPath: uffdSock},
		ResumeVM:     true,
	}
	if a.chunked() {
		load.SnapshotPath = a.fcSnapPath(sb, "snapfile-"+sb.snapLayer)
		load.MemBackend.BackendPath = a.fcSnapPath(sb, "uffd.sock")
		load.TrackDirtyPages = true // keep Diff pauses possible after restore
		// 校时: re-arm the guest realtime clock. Firecracker rejects the
		// flag on aarch64 ("clock_realtime is not supported"); those guests
		// re-sync via the guestd resume hook instead.
		load.ClockRealtime = runtime.GOARCH == "amd64"
	}
	if err := c.LoadSnapshot(ctx, load); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if sb.hotplugTotalMiB() > 0 {
		// The plugged size is checkpoint-time state riding the snapfile, and
		// the checkpoint being restored is not necessarily the last one this
		// process saw (fork restores the parent's checkpoint, rollback an
		// older one). Read the truth back from the device.
		if st, err := c.GetMemoryHotplug(ctx); err != nil {
			log.Printf("nodeagent: hotplug state after restore %s: %v", sb.id, err)
		} else {
			a.mu.Lock()
			sb.memMiB = sb.baseMemMiB + st.PluggedSizeMiB
			a.mu.Unlock()
		}
	}
	if err := sb.machine.To(lifecycle.StateRunning); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if err := a.waitGuest(ctx, sb, 15*time.Second); err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("guestd unreachable after resume: %w", err)
	}
	// Notify the guest (resume counter, /etc/embervm/resume-hook). Old
	// guestd builds without the endpoint make this a no-op.
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	_, _ = sb.guest.Resumed(rctx)
	cancel()
	if a.chunked() && a.cfg.PauseBalloonSettle > 0 {
		// The snapshot carries the pause-time inflation; hand the memory
		// back so the guest is not running squeezed.
		if err := c.PatchBalloon(ctx, 0); err != nil {
			log.Printf("nodeagent: balloon deflate %s: %v", sb.id, err)
		}
	}
	return a.status(sb), nil
}

// SnapshotSandbox pauses, snapshots, and resumes (a caller-visible snapshot).
func (a *Agent) SnapshotSandbox(ctx context.Context, sandboxID, tag string) (string, error) {
	if err := a.PauseSandbox(ctx, sandboxID); err != nil {
		return "", err
	}
	if _, err := a.resume(ctx, sandboxID); err != nil {
		return "", err
	}
	sb, err := a.get(sandboxID)
	if err != nil {
		return "", err
	}
	return sandboxID + "@" + tag + "-" + strconv.Itoa(sb.snapCount), nil
}

// StopSandbox tears the sandbox down and releases its resources.
func (a *Agent) StopSandbox(ctx context.Context, sandboxID string) error {
	sb, err := a.get(sandboxID)
	if err != nil {
		return err
	}
	_ = sb.machine.To(lifecycle.StateStopping)
	a.cleanup(ctx, sb)
	_ = sb.machine.To(lifecycle.StateStopped)
	a.mu.Lock()
	delete(a.sbx, sandboxID)
	a.mu.Unlock()
	return nil
}

// Status returns the current sandbox status.
func (a *Agent) Status(_ context.Context, sandboxID string) (nodeapi.SandboxStatus, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	return a.status(sb), nil
}

// Exec runs a command in the guest via guestd.
func (a *Agent) Exec(ctx context.Context, sandboxID string, req *guestapi.ExecRequest) (*guestapi.ExecResponse, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nil, err
	}
	return sb.guest.Exec(ctx, req)
}

// Health probes guestd.
func (a *Agent) Health(ctx context.Context, sandboxID string) (*guestapi.HealthResponse, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nil, err
	}
	return sb.guest.Health(ctx)
}

// ReadFile reads a guest file via guestd.
func (a *Agent) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nil, err
	}
	return sb.guest.ReadFile(ctx, path)
}

// WriteFile writes a guest file via guestd.
func (a *Agent) WriteFile(ctx context.Context, sandboxID, path string, mode fs.FileMode, data []byte) error {
	sb, err := a.get(sandboxID)
	if err != nil {
		return err
	}
	return sb.guest.WriteFile(ctx, path, mode, data)
}

// --- helpers ---------------------------------------------------------------

// Healthz reports capacity for the scheduler's poll.
func (a *Agent) Healthz(_ context.Context) (nodeapi.NodeHealth, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	used := 0
	for _, sb := range a.sbx {
		st := sb.machine.State()
		if st == lifecycle.StateRunning || st == lifecycle.StateStarting ||
			st == lifecycle.StateResuming || st == lifecycle.StatePausing {
			used += sb.memMiB
		}
	}
	h := nodeapi.NodeHealth{
		CapacityMiB:     a.cfg.CapacityMiB,
		UsedMiB:         used,
		Sandboxes:       len(a.sbx),
		CPUCores:        runtime.NumCPU(),
		FailedSandboxes: a.failed,
	}
	a.failed = nil // reported once; the control plane owns it now
	return h, nil
}

// DialGuest opens a TCP connection to a guest port from inside the
// sandbox's network namespace — the data path of the M4 gateway proxy.
func (a *Agent) DialGuest(ctx context.Context, sandboxID string, port int) (net.Conn, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nil, err
	}
	return sb.lease.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", sb.lease.GuestIP, port))
}

// resizeMemory retargets the virtio-mem region so the sandbox's effective
// memory becomes targetMiB (base ≤ target ≤ ceiling), then polls until the
// guest driver converges. Growth must converge — a stuck driver is an error.
// Shrink is guest-cooperative by design (the kernel must migrate pages out
// of the blocks): a partial unplug is reported as the achieved size, not an
// error. The achieved effective size is recorded on the sandbox either way.
func (a *Agent) resizeMemory(ctx context.Context, sb *sandbox, targetMiB int) (int, error) {
	if targetMiB < sb.baseMemMiB || targetMiB > sb.maxMemMiB {
		return sb.memMiB, fmt.Errorf("resize %s: target %d MiB outside [%d, %d]",
			sb.id, targetMiB, sb.baseMemMiB, sb.maxMemMiB)
	}
	grow := targetMiB > sb.memMiB
	c := fcclient.New(a.fcAPISock(sb))
	want := targetMiB - sb.baseMemMiB
	if err := c.PatchMemoryHotplug(ctx, want); err != nil {
		return sb.memMiB, err
	}
	deadline := time.Now().Add(30 * time.Second)
	var st fcclient.MemoryHotplugStatus
	for {
		var err error
		if st, err = c.GetMemoryHotplug(ctx); err != nil {
			return sb.memMiB, err
		}
		if st.PluggedSizeMiB == want || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return sb.memMiB, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	achieved := sb.baseMemMiB + st.PluggedSizeMiB
	a.mu.Lock()
	sb.memMiB = achieved
	a.mu.Unlock()
	if grow && achieved != targetMiB {
		return achieved, fmt.Errorf("resize %s: guest plugged %d/%d MiB before deadline",
			sb.id, st.PluggedSizeMiB, want)
	}
	return achieved, nil
}

// ResizeSandbox retargets a RUNNING sandbox's effective geometry within its
// create-time ceilings (M6): memory via the virtio-mem region (growth
// requires guest convergence; shrink is cooperative and may land above the
// ask), CPU via the cgroup cpu.max quota. Memory is applied first — it is
// the operation that can fail halfway; the result always reports the
// achieved values so the control plane can reconcile its accounting.
func (a *Agent) ResizeSandbox(ctx context.Context, sandboxID string, req nodeapi.ResizeRequest) (nodeapi.ResizeResult, error) {
	sb, err := a.get(sandboxID)
	if err != nil {
		return nodeapi.ResizeResult{}, err
	}
	res := nodeapi.ResizeResult{MemoryMiB: sb.memMiB, VCPUs: sb.vcpus}
	if st := sb.machine.State(); st != lifecycle.StateRunning {
		return res, fmt.Errorf("resize %s: state %s, want RUNNING", sandboxID, st)
	}
	if req.VCPUs != 0 && (req.VCPUs < 1 || req.VCPUs > sb.maxVCPUs) {
		return res, fmt.Errorf("resize %s: vcpus %d outside [1, %d]", sandboxID, req.VCPUs, sb.maxVCPUs)
	}
	if req.MemoryMiB != 0 {
		achieved, err := a.resizeMemory(ctx, sb, req.MemoryMiB)
		res.MemoryMiB = achieved
		if err != nil {
			return res, err
		}
	}
	if req.VCPUs != 0 && req.VCPUs != sb.vcpus {
		if err := a.writeCPUMax(sb.id, req.VCPUs); err != nil {
			return res, fmt.Errorf("resize %s: cpu.max: %w", sandboxID, err)
		}
		a.mu.Lock()
		sb.vcpus = req.VCPUs
		a.mu.Unlock()
		res.VCPUs = req.VCPUs
	}
	return res, nil
}

// SetBalloon retargets a running sandbox's balloon device.
func (a *Agent) SetBalloon(ctx context.Context, sandboxID string, targetMiB int) error {
	sb, err := a.get(sandboxID)
	if err != nil {
		return err
	}
	if st := sb.machine.State(); st != lifecycle.StateRunning {
		return fmt.Errorf("balloon %s: state %s, want RUNNING", sandboxID, st)
	}
	return fcclient.New(a.fcAPISock(sb)).PatchBalloon(ctx, targetMiB)
}

// WorkDirOf returns a sandbox's runtime directory (tests and debugging).
func (a *Agent) WorkDirOf(sandboxID string) string {
	return filepath.Join(a.cfg.WorkDir, sandboxID)
}

// PidsOf returns a sandbox's Firecracker and uffd handler pids (0 = no such
// process); tests kill them behind the agent's back to exercise the watchdog.
func (a *Agent) PidsOf(sandboxID string) (fcPid, uffdPid int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sb, ok := a.sbx[sandboxID]
	if !ok {
		return 0, 0
	}
	if sb.fc != nil && sb.fc.Process != nil {
		fcPid = sb.fc.Process.Pid
	}
	if sb.uffd != nil && sb.uffd.Process != nil {
		uffdPid = sb.uffd.Process.Pid
	}
	return fcPid, uffdPid
}

func (a *Agent) get(id string) (*sandbox, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sb, ok := a.sbx[id]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", id)
	}
	return sb, nil
}

func (a *Agent) status(sb *sandbox) nodeapi.SandboxStatus {
	a.mu.Lock()
	mem, vcpus := sb.memMiB, sb.vcpus
	a.mu.Unlock()
	return nodeapi.SandboxStatus{
		SandboxID: sb.id,
		State:     string(sb.machine.State()),
		GuestAddr: fmt.Sprintf("%s:%d", sb.lease.GuestIP, guestapi.Port),
		Netns:     sb.lease.Netns,
		MemoryMiB: mem,
		VCPUs:     vcpus,
	}
}

// guestClient builds a guestapi client whose HTTP transport dials into the
// sandbox netns.
func (a *Agent) guestClient(sb *sandbox) *guestapi.Client {
	hc := &http.Client{Transport: &http.Transport{DialContext: sb.lease.DialContext}}
	return guestapi.NewClient(fmt.Sprintf("http://%s:%d", sb.lease.GuestIP, guestapi.Port), hc)
}

// waitGuest polls guestd /healthz until it answers, the deadline passes, or
// the caller's context is canceled.
func (a *Agent) waitGuest(ctx context.Context, sb *sandbox, timeout time.Duration) error {
	if sb.guest == nil {
		sb.guest = a.guestClient(sb)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := sb.guest.Health(cctx)
		cancel()
		if err == nil {
			return nil
		}
		// Fine-grained so resume-readiness (and its measured latency) is not
		// quantized coarsely; the exit-criteria hot-resume budget is <1s.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout after %s", timeout)
}

// killFC and killUffd swap the process pointer out under a.mu — the watchdog
// and PidsOf read sb.fc/sb.uffd under the same lock — and kill/reap outside
// it so a slow Wait cannot stall every other agent verb.
func (a *Agent) killFC(sb *sandbox) {
	a.mu.Lock()
	fc := sb.fc
	sb.fc = nil
	a.mu.Unlock()
	if fc != nil && fc.Process != nil {
		_ = fc.Process.Kill()
		_, _ = fc.Process.Wait()
	}
}

func (a *Agent) killUffd(sb *sandbox) {
	a.mu.Lock()
	uffd := sb.uffd
	sb.uffd = nil
	a.mu.Unlock()
	if uffd != nil && uffd.Process != nil {
		_ = uffd.Process.Kill()
		_, _ = uffd.Process.Wait()
	}
}

// cleanup kills processes, releases the netns lease, removes the cgroup, and
// destroys storage. Safe to call on a partially-constructed sandbox.
func (a *Agent) cleanup(ctx context.Context, sb *sandbox) {
	a.killFC(sb)
	a.killUffd(sb)
	a.clearEgress(ctx, sb)
	if a.jailed() {
		a.teardownJail(sb)
	}
	a.removeCgroup(sb.id)
	sb.lease.Release()
	_ = a.cfg.Storage.DestroySandbox(ctx, sb.id)
}

// waitSocket waits for a unix socket file to appear.
func waitSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&fs.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for socket %s", path)
}

// waitSocketReady waits until the unix socket ACCEPTS a connection. The
// stat-based waitSocket races Firecracker's bind()→listen() window: under a
// boot storm the socket file exists while connect(2) still refuses (seen at
// 50-way create concurrency). Only for sockets whose owner tolerates a probe
// connection (the FC API server); the uffd socket expects exactly one peer —
// Firecracker — and must keep the stat-based wait.
func waitSocketReady(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for socket %s to accept: %w", path, lastErr)
}
