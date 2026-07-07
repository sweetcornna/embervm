package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrReplicationUnsupported marks backends without snapshot replication
// (PlainBackend): cross-node restore and disk write-through require ZFS.
var ErrReplicationUnsupported = errors.New("storage: backend does not support replication")

// Replicator moves dataset snapshots between nodes as `zfs send` streams
// stored in the L1 object store. GUID lineage matters: a receiving node can
// only apply an incremental stream if it holds the base snapshot from the
// SAME send chain — templates must therefore be distributed as streams
// (ReceiveTemplate), never rebuilt per node.
type Replicator interface {
	// SendTemplate streams the template dataset (its @final snapshot).
	SendTemplate(ctx context.Context, templateID string, w io.Writer) error
	// ReceiveTemplate materializes a template dataset from SendTemplate's
	// stream. Idempotent: an existing template is left untouched.
	ReceiveTemplate(ctx context.Context, templateID string, r io.Reader) error
	// SendSnapshotDelta streams sandbox changes up to toTag: from the
	// template origin when fromTag is empty (first pause), else from the
	// previous pause snapshot.
	SendSnapshotDelta(ctx context.Context, sandboxID, fromTag, toTag string, w io.Writer) error
	// ReceiveSnapshotDelta applies one delta stream in order; the first
	// call clones lineage off the local template dataset.
	ReceiveSnapshotDelta(ctx context.Context, sandboxID, templateID string, r io.Reader) error
	// SetSandboxMountpoint pins the dataset mountpoint. A Firecracker
	// snapfile records absolute drive paths, so a restored sandbox's
	// dataset must mount exactly where the origin node's did.
	SetSandboxMountpoint(ctx context.Context, sandboxID, mountpoint string) error
}

var _ Replicator = (*ZFSBackend)(nil)

// streamRunner runs a command with wired stdin/stdout (zfs send | receive).
// Injectable for unit tests.
type streamRunner func(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error

func execStream(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout = stdin, stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

func (b *ZFSBackend) stream(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	srun := b.srun
	if srun == nil {
		srun = execStream
	}
	return srun(ctx, stdin, stdout, "zfs", args...)
}

// SendTemplate implements Replicator. -c keeps blocks compressed on the wire.
func (b *ZFSBackend) SendTemplate(ctx context.Context, templateID string, w io.Writer) error {
	if err := validateID("template", templateID); err != nil {
		return err
	}
	return b.stream(ctx, nil, w, "send", "-c", b.templateDS(templateID)+"@final")
}

// ReceiveTemplate implements Replicator.
func (b *ZFSBackend) ReceiveTemplate(ctx context.Context, templateID string, r io.Reader) error {
	if err := validateID("template", templateID); err != nil {
		return err
	}
	ds := b.templateDS(templateID)
	if b.datasetExists(ctx, ds+"@final") {
		return nil
	}
	// zfs receive does not create ancestors; a node that never built a
	// template has no templates/ container yet.
	if parent := b.pool + "/templates"; !b.datasetExists(ctx, parent) {
		if _, err := b.run(ctx, "zfs", "create", "-p", parent); err != nil {
			return err
		}
	}
	return b.stream(ctx, r, nil, "receive", ds)
}

// SendSnapshotDelta implements Replicator.
func (b *ZFSBackend) SendSnapshotDelta(ctx context.Context, sandboxID, fromTag, toTag string, w io.Writer) error {
	if err := validateID("sandbox", sandboxID); err != nil {
		return err
	}
	if err := validateID("tag", toTag); err != nil {
		return err
	}
	sds := b.sandboxDS(sandboxID)
	base := "@" + fromTag
	if fromTag == "" {
		// First delta: incremental from the clone's origin snapshot.
		origin, err := b.run(ctx, "zfs", "get", "-H", "-o", "value", "origin", sds)
		if err != nil {
			return err
		}
		origin = strings.TrimSpace(origin)
		if origin == "" || origin == "-" {
			return fmt.Errorf("sandbox %s has no clone origin; cannot build delta chain", sandboxID)
		}
		base = origin
	} else if err := validateID("tag", fromTag); err != nil {
		return err
	}
	return b.stream(ctx, nil, w, "send", "-c", "-i", base, sds+"@"+toTag)
}

// ReceiveSnapshotDelta implements Replicator.
func (b *ZFSBackend) ReceiveSnapshotDelta(ctx context.Context, sandboxID, templateID string, r io.Reader) error {
	if err := validateID("sandbox", sandboxID); err != nil {
		return err
	}
	if err := validateID("template", templateID); err != nil {
		return err
	}
	if parent := b.pool + "/sandboxes"; !b.datasetExists(ctx, parent) {
		if _, err := b.run(ctx, "zfs", "create", "-p", parent); err != nil {
			return err
		}
	}
	sds := b.sandboxDS(sandboxID)
	if !b.datasetExists(ctx, sds) {
		return b.stream(ctx, r, nil, "receive",
			"-o", "origin="+b.templateDS(templateID)+"@final", sds)
	}
	// -F rolls back to the latest received snapshot first, so replaying a
	// chain over a partially-restored dataset stays deterministic.
	return b.stream(ctx, r, nil, "receive", "-F", sds)
}

// SetSandboxMountpoint implements Replicator.
func (b *ZFSBackend) SetSandboxMountpoint(ctx context.Context, sandboxID, mountpoint string) error {
	if err := validateID("sandbox", sandboxID); err != nil {
		return err
	}
	if mountpoint == "" || !strings.HasPrefix(mountpoint, "/") {
		return fmt.Errorf("bad mountpoint %q", mountpoint)
	}
	_, err := b.run(ctx, "zfs", "set", "mountpoint="+mountpoint, b.sandboxDS(sandboxID))
	return err
}
