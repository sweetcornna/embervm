// Package storage abstracts per-sandbox disk provisioning for the node
// agent: importing a built rootfs as an immutable template and cloning it
// into writable sandbox storage (a rootfs clone plus a sparse data disk).
//
// The production backend is ZFS (O(1) clones and snapshots, master-spec D5);
// PlainBackend is a portable copy-based implementation for `embervm dev` and
// tests. The data disk is always a raw file on the dataset — never a zvol
// (docs/zh/04 §1) — and is created sparse so a 15GiB disk neither bloats
// snapshots nor enters the resume critical path (docs/zh/02 §1).
package storage

import (
	"context"
	"fmt"
	"regexp"
)

// SandboxPaths locates a sandbox's on-disk artifacts.
type SandboxPaths struct {
	Dir        string // dataset mountpoint / directory holding the sandbox files
	RootfsExt4 string // writable rootfs clone the guest boots from
	DataRaw    string // sparse data disk attached as a second drive
}

// Backend provisions template and sandbox storage.
type Backend interface {
	// EnsureTemplate imports rootfsExt4Src as immutable template storage and
	// makes it cloneable (ZFS @final snapshot). Idempotent: a second call
	// with the same templateID is a no-op success.
	EnsureTemplate(ctx context.Context, templateID, rootfsExt4Src string) error

	// CloneSandbox creates a sandbox's storage from a template: a writable
	// rootfs clone plus a fresh sparse data.raw of dataDiskGiB. Returns the
	// sandbox paths.
	CloneSandbox(ctx context.Context, sandboxID, templateID string, dataDiskGiB int) (SandboxPaths, error)

	// Paths returns where a sandbox's artifacts live (without touching disk).
	Paths(sandboxID string) SandboxPaths

	// Snapshot takes a point-in-time snapshot of the sandbox storage (pause
	// path) and returns an opaque snapshot identifier.
	Snapshot(ctx context.Context, sandboxID, tag string) (string, error)

	// DestroySandbox removes a sandbox's storage and its snapshots.
	// Idempotent: destroying an absent sandbox is success.
	DestroySandbox(ctx context.Context, sandboxID string) error
}

// idRE constrains IDs to safe dataset/path components.
var idRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func validateID(kind, id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid %s id %q: must match %s", kind, id, idRE.String())
	}
	return nil
}
