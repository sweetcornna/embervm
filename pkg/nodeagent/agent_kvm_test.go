//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/embervm/embervm/pkg/guestapi"
	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// TestAgentLifecycleKVM boots a real template microVM and drives the full M1
// lifecycle: create → guestd health(seq 1) → exec → file R/W → pause → resume
// → guestd health(seq 2, proving the SAME process survived) → stop. It is
// gated behind EMBERVM_KVM_TESTS=1 and needs root, /dev/kvm, and asset paths
// supplied by the CI job; anything missing SKIPs.
func TestAgentLifecycleKVM(t *testing.T) {
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run the KVM lifecycle test")
	}
	if os.Geteuid() != 0 {
		t.Skip("KVM lifecycle test needs root")
	}
	env := func(k string) string { return os.Getenv(k) }
	kernel, fcBin := env("EMBERVM_KERNEL"), env("EMBERVM_FC_BIN")
	uffdBin, guestdBin := env("EMBERVM_UFFD_BIN"), env("EMBERVM_GUESTD_BIN")
	scriptDir := env("EMBERVM_SCRIPT_DIR")
	image := env("EMBERVM_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	for name, p := range map[string]string{
		"EMBERVM_KERNEL": kernel, "EMBERVM_FC_BIN": fcBin,
		"EMBERVM_UFFD_BIN": uffdBin, "EMBERVM_GUESTD_BIN": guestdBin,
		"EMBERVM_SCRIPT_DIR": scriptDir,
	} {
		if p == "" {
			t.Skipf("%s not set", name)
		}
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s=%s not found: %v", name, p, err)
		}
	}

	ctx := context.Background()
	pool := netns.NewPool(scriptDir, 2)
	if err := pool.Setup(ctx); err != nil {
		t.Fatalf("netns pool setup: %v", err)
	}
	t.Cleanup(func() { _ = pool.Teardown(ctx) })

	agent, err := nodeagent.New(nodeagent.Config{
		Storage:        storage.NewPlainBackend(t.TempDir()),
		Pool:           pool,
		WorkDir:        t.TempDir(),
		KernelPath:     kernel,
		FCBin:          fcBin,
		UffdHandlerBin: uffdBin,
		GuestdBin:      guestdBin,
		RestoreMode:    "prefetch",
	})
	if err != nil {
		t.Fatalf("nodeagent.New: %v", err)
	}

	if err := agent.BuildTemplate(ctx, "t1", image); err != nil {
		t.Fatalf("BuildTemplate(%s): %v", image, err)
	}

	st, err := agent.CreateSandbox(ctx, nodeapi.CreateSandboxRequest{
		SandboxID: "s1", TemplateID: "t1", VCPUs: 1, MemoryMiB: 256, DataDiskGiB: 1,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if st.State != "RUNNING" {
		t.Fatalf("state after create = %s, want RUNNING", st.State)
	}
	t.Cleanup(func() { _ = agent.StopSandbox(context.Background(), "s1") })

	if _, err := agent.Health(ctx, "s1"); err != nil {
		t.Fatalf("Health after create: %v", err)
	}

	ex, err := agent.Exec(ctx, "s1", &guestapi.ExecRequest{Cmd: "echo", Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if ex.ExitCode != 0 || string(ex.Stdout) != "hello\n" {
		t.Errorf("exec = %+v, want exit 0 stdout %q", ex, "hello\n")
	}

	if err := agent.WriteFile(ctx, "s1", "/tmp/x", 0o644, []byte("payload")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := agent.ReadFile(ctx, "s1", "/tmp/x")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("read = %q, want payload", data)
	}

	// Baseline the per-process health counter right before pausing. Only
	// successful probes increment it, so a genuine restore of the SAME
	// process keeps climbing, whereas a guest reboot would reset it low.
	hBefore, err := agent.Health(ctx, "s1")
	if err != nil {
		t.Fatalf("Health before pause: %v", err)
	}

	pauseStart := time.Now()
	if err := agent.PauseSandbox(ctx, "s1"); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	if _, err := agent.ResumeSandbox(ctx, "s1"); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}
	t.Logf("pause+resume round trip: %v", time.Since(pauseStart))

	hAfter, err := agent.Health(ctx, "s1")
	if err != nil {
		t.Fatalf("Health after resume: %v", err)
	}
	// Monotonic across the restore => the same guestd process survived.
	// (ResumeSandbox's readiness probe also increments, so After is at least
	// Before+2; a reboot would have reset the counter below Before.)
	if hAfter.Seq <= hBefore.Seq {
		t.Errorf("health seq %d -> %d across restore: not monotonic, guest process did NOT survive (reset on reboot?)",
			hBefore.Seq, hAfter.Seq)
	}

	if err := agent.StopSandbox(ctx, "s1"); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
}
