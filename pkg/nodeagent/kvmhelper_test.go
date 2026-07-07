//go:build linux

package nodeagent_test

import (
	"context"
	"os"
	"testing"

	"github.com/embervm/embervm/pkg/netns"
	"github.com/embervm/embervm/pkg/nodeagent"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

// kvmAgent builds a node agent from the asset paths the CI job exports,
// skipping when the KVM prerequisites or paths are absent. poolSize sizes the
// netns pool. Returns the agent, the template image to use, and a context.
func kvmAgent(t *testing.T, poolSize int) (nodeapi.Agent, string) {
	t.Helper()
	if os.Getenv("EMBERVM_KVM_TESTS") != "1" {
		t.Skip("set EMBERVM_KVM_TESTS=1 to run KVM tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("KVM tests need root")
	}
	get := os.Getenv
	kernel, fcBin := get("EMBERVM_KERNEL"), get("EMBERVM_FC_BIN")
	uffdBin, guestdBin := get("EMBERVM_UFFD_BIN"), get("EMBERVM_GUESTD_BIN")
	scriptDir := get("EMBERVM_SCRIPT_DIR")
	image := get("EMBERVM_TEST_IMAGE")
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

	pool := netns.NewPool(scriptDir, poolSize)
	if err := pool.Setup(context.Background()); err != nil {
		t.Fatalf("netns pool setup: %v", err)
	}
	t.Cleanup(func() { _ = pool.Teardown(context.Background()) })

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
	return agent, image
}
