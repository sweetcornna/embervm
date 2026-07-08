//go:build linux

package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/embervm/embervm/pkg/chunkstore"
	"github.com/embervm/embervm/pkg/fcclient"
	"github.com/embervm/embervm/pkg/lifecycle"
	"github.com/embervm/embervm/pkg/memsnap"
	"github.com/embervm/embervm/pkg/nodeapi"
	"github.com/embervm/embervm/pkg/storage"
)

func (sb *sandbox) snapDir() string      { return filepath.Join(sb.dir, "snap") }
func (sb *sandbox) wsPath() string       { return filepath.Join(sb.snapDir(), "ws.json") }
func (sb *sandbox) layerID(n int) string { return "p" + strconv.Itoa(n) }
func (sb *sandbox) snapfile(l string) string {
	return filepath.Join(sb.snapDir(), "snapfile-"+l)
}

func (a *Agent) chunked() bool { return a.cfg.RestoreMode == "chunked" }

// pauseChunked runs the M2 pause pipeline after the VM has been paused:
// Full (first) / Diff (later) snapshot -> chunkify into the local store ->
// dataset snapshot -> write-through everything to L1. The raw memfile is
// deleted once chunkified: the chunk store is the source of truth.
func (a *Agent) pauseChunked(ctx context.Context, sb *sandbox) error {
	snapDir := sb.snapDir()
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	layerN := sb.snapCount + 1
	layerID := sb.layerID(layerN)
	memfile := filepath.Join(snapDir, "memfile-"+layerID)
	// A fresh sandbox — or one whose chain was reset by a cold restore
	// (the synthetic-full parent lives in the cold store, ADR-0004 D7) —
	// roots a new Full chain; otherwise pause diffs against the chain.
	snapType := "Full"
	if len(sb.layers) > 0 && !sb.forceFullPause {
		snapType = "Diff"
	}

	c := fcclient.New(filepath.Join(sb.dir, "fc.sock"))
	if err := c.CreateSnapshot(ctx, fcclient.SnapshotCreate{
		SnapshotType: snapType,
		SnapshotPath: sb.snapfile(layerID),
		MemFilePath:  memfile,
	}); err != nil {
		return err
	}
	a.killFC(sb)
	// Let the previous resume's handler exit gracefully so it can persist a
	// freshly recorded working set before we push ws.json to L1.
	a.drainUffd(sb, 5*time.Second)

	sink := chunkstore.Bytes{Ctx: ctx, S: a.localStore}
	// Parent chunks for diff merging must read through L1: after a warm
	// restore the local cache holds only what the handler fetched so far
	// (backfill may still be running when the next pause lands).
	getter := sink
	if a.l1 != nil {
		getter = chunkstore.Bytes{Ctx: ctx, S: chunkstore.Tiered{Local: a.localStore, Remote: a.l1}}
	}
	opts := memsnap.WriteOptions{
		LayerID:       layerID,
		FCVersion:     a.cfg.FCVersion,
		KernelVersion: a.cfg.KernelVersion,
	}
	var (
		m   *memsnap.Manifest
		err error
	)
	if snapType == "Full" {
		m, err = memsnap.WriteLayer(memfile, opts, sink)
	} else {
		opts.Parent = sb.layerID(layerN - 1)
		var parent *memsnap.View
		parent, err = memsnap.Resolve(sb.layers)
		if err != nil {
			return fmt.Errorf("resolve parent chain: %w", err)
		}
		m, err = memsnap.WriteDiffLayer(memfile, opts, parent, getter, sink)
	}
	if err != nil {
		return fmt.Errorf("chunkify %s: %w", layerID, err)
	}
	if snapType == "Full" {
		// A fresh chain root: manifests from earlier epochs (e.g. the
		// synthetic layer-cold.json a cold restore fetched) must not leak
		// into the handler's layer-*.json glob — two full layers cannot
		// resolve.
		stale, _ := filepath.Glob(filepath.Join(snapDir, "layer-*.json"))
		for _, f := range stale {
			_ = os.Remove(f)
		}
	}
	if err := m.WriteFile(filepath.Join(snapDir, "layer-"+layerID+".json")); err != nil {
		return err
	}
	_ = os.Remove(memfile)
	if snapType == "Full" {
		sb.layers = []*memsnap.Manifest{m} // new chain root
		sb.forceFullPause = false
	} else {
		sb.layers = append(sb.layers, m)
	}
	sb.snapCount = layerN

	prevDisk := ""
	if n := len(sb.diskLayers); n > 0 {
		prevDisk = sb.diskLayers[n-1]
	}
	if _, err := a.cfg.Storage.Snapshot(ctx, sb.id, layerID); err != nil {
		return err
	}
	sb.diskLayers = append(sb.diskLayers, layerID)
	sb.snapLayer = layerID
	if a.l1 != nil {
		if err := a.pushL1(ctx, sb, m, layerID, prevDisk); err != nil {
			// Write-through is the RPO guarantee (docs/zh/02 §3): a pause
			// that did not reach L1 is not durable, so fail loudly.
			return fmt.Errorf("write-through L1: %w", err)
		}
	}
	return nil
}

// pushL1 uploads the new layer's chunks, manifest, snapfile, WS trace, disk
// delta, and the refreshed restore descriptor.
func (a *Agent) pushL1(ctx context.Context, sb *sandbox, m *memsnap.Manifest, layerID, prevDisk string) error {
	var hashes []string
	for _, c := range m.Chunks {
		if !c.Zero {
			hashes = append(hashes, c.Hash)
		}
	}
	if _, err := (chunkstore.Copier{Src: a.localStore, Dst: a.l1}).Copy(ctx, hashes); err != nil {
		return err
	}
	if err := a.putFile(ctx, KeyLayer(sb.id, layerID), filepath.Join(sb.snapDir(), "layer-"+layerID+".json")); err != nil {
		return err
	}
	if err := a.putFile(ctx, KeySnapfile(sb.id, layerID), sb.snapfile(layerID)); err != nil {
		return err
	}
	hasWS := false
	if _, err := os.Stat(sb.wsPath()); err == nil {
		hasWS = true
		if err := a.putFile(ctx, KeyWS(sb.id), sb.wsPath()); err != nil {
			return err
		}
	}
	if repl, ok := a.cfg.Storage.(storage.Replicator); ok {
		if err := a.putStream(ctx, KeyDiskDelta(sb.id, layerID), func(w io.Writer) error {
			return repl.SendSnapshotDelta(ctx, sb.id, prevDisk, layerID, w)
		}); err != nil {
			return err
		}
	}
	desc := SnapshotDescriptor{
		FormatVersion: 1,
		SandboxID:     sb.id,
		TemplateID:    sb.templateID,
		VCPUs:         sb.vcpus,
		MemoryMiB:     sb.memMiB,
		DataDiskGiB:   sb.dataDiskGiB,
		Dir:           sb.mountDir,
		HasWS:         hasWS,
		Tier:          "warm",
		DiskLayers:    sb.diskLayers,
		SnapSeq:       sb.snapCount,
	}
	for _, lm := range sb.layers {
		desc.Layers = append(desc.Layers, lm.LayerID)
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return err
	}
	return a.l1.PutObject(ctx, KeySnapshotJSON(sb.id), bytes.NewReader(data), int64(len(data)))
}

// pushTemplateL1 publishes the template dataset stream once (GUID lineage:
// receiving nodes must clone off THIS stream, not a local rebuild).
func (a *Agent) pushTemplateL1(ctx context.Context, templateID string) error {
	repl, ok := a.cfg.Storage.(storage.Replicator)
	if !ok || a.l1 == nil {
		return nil
	}
	key := KeyTemplateStream(templateID)
	if ok, err := a.l1.HasObject(ctx, key); err != nil || ok {
		return err
	}
	return a.putStream(ctx, key, func(w io.Writer) error {
		return repl.SendTemplate(ctx, templateID, w)
	})
}

// RestoreSandbox rebuilds a sandbox this agent may never have seen from the
// tier's object store ("warm" = L1, "cold" = the cold store): template
// stream -> disk delta chain -> manifests/snapfile/WS -> normal chunked
// resume with a cold local chunk cache. Cross-node placement stays
// test/scheduler driven until M4.
func (a *Agent) RestoreSandbox(ctx context.Context, sandboxID, tier string) (nodeapi.SandboxStatus, error) {
	src, err := a.tierStore(tier)
	if err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: %w", sandboxID, err)
	}
	repl, ok := a.cfg.Storage.(storage.Replicator)
	if !ok {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: %w", sandboxID, storage.ErrReplicationUnsupported)
	}
	if !a.chunked() {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: requires restore_mode=chunked", sandboxID)
	}

	var desc SnapshotDescriptor
	if err := getJSONFrom(ctx, src, KeySnapshotJSON(sandboxID), &desc); err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: descriptor: %w", sandboxID, err)
	}
	if desc.FormatVersion != 1 || len(desc.Layers) == 0 {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: bad descriptor %+v", sandboxID, desc)
	}
	if desc.Tier != "" && desc.Tier != tier {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: descriptor tier %q != requested %q", sandboxID, desc.Tier, tier)
	}
	diskLayers := desc.DiskLayers
	if len(diskLayers) == 0 {
		diskLayers = desc.Layers // M2 descriptors: disk chain mirrors memory chain
	}

	// Template lineage always ships via L1 (templates are node-global and
	// never archived); the disk delta chain comes from the tier store.
	if err := a.receiveObject(ctx, KeyTemplateStream(desc.TemplateID), func(r io.Reader) error {
		return repl.ReceiveTemplate(ctx, desc.TemplateID, r)
	}); err != nil {
		return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: template: %w", sandboxID, err)
	}
	for _, layer := range diskLayers {
		rc, err := src.GetObject(ctx, KeyDiskDelta(sandboxID, layer))
		if err != nil {
			return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: disk %s: %w", sandboxID, layer, err)
		}
		err = repl.ReceiveSnapshotDelta(ctx, sandboxID, desc.TemplateID, rc)
		rc.Close()
		if err != nil {
			return nodeapi.SandboxStatus{}, fmt.Errorf("restore %s: receive %s: %w", sandboxID, layer, err)
		}
	}
	// The snapfile records absolute drive paths from the origin node.
	if err := repl.SetSandboxMountpoint(ctx, sandboxID, desc.Dir); err != nil {
		return nodeapi.SandboxStatus{}, err
	}

	snapSeq := desc.SnapSeq
	if snapSeq == 0 {
		snapSeq = len(desc.Layers)
	}
	sb := &sandbox{
		id:          sandboxID,
		machine:     lifecycle.New(lifecycle.StatePausedHot),
		dir:         filepath.Join(a.cfg.WorkDir, sandboxID),
		vcpus:       desc.VCPUs,
		memMiB:      desc.MemoryMiB,
		templateID:  desc.TemplateID,
		dataDiskGiB: desc.DataDiskGiB,
		mountDir:    desc.Dir,
		rootfs:      filepath.Join(desc.Dir, "rootfs.ext4"),
		dataRaw:     filepath.Join(desc.Dir, "data.raw"),
		snapCount:   snapSeq,
		diskLayers:  diskLayers,
		restoreTier: tier,
		// A cold snapshot's chunks live only in the cold store; the next
		// pause roots a fresh Full chain back in L1 (ADR-0004 D7).
		forceFullPause: tier == "cold",
	}
	if err := os.MkdirAll(sb.snapDir(), 0o755); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	for _, layer := range desc.Layers {
		local := filepath.Join(sb.snapDir(), "layer-"+layer+".json")
		if err := a.fetchFileFrom(ctx, src, KeyLayer(sandboxID, layer), local); err != nil {
			return nodeapi.SandboxStatus{}, err
		}
		m, err := memsnap.ReadManifest(local)
		if err != nil {
			return nodeapi.SandboxStatus{}, err
		}
		sb.layers = append(sb.layers, m)
	}
	last := desc.Layers[len(desc.Layers)-1]
	sb.snapLayer = last
	if err := a.fetchFileFrom(ctx, src, KeySnapfile(sandboxID, last), sb.snapfile(last)); err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	if desc.HasWS {
		if err := a.fetchFileFrom(ctx, src, KeyWS(sandboxID), sb.wsPath()); err != nil {
			return nodeapi.SandboxStatus{}, err
		}
	}

	lease, err := a.cfg.Pool.Acquire()
	if err != nil {
		return nodeapi.SandboxStatus{}, err
	}
	sb.lease = lease

	a.mu.Lock()
	a.sbx[sandboxID] = sb
	a.mu.Unlock()

	st, err := a.ResumeSandbox(ctx, sandboxID)
	if err != nil {
		a.mu.Lock()
		delete(a.sbx, sandboxID)
		a.mu.Unlock()
		a.cleanup(ctx, sb)
		return nodeapi.SandboxStatus{}, err
	}
	return st, nil
}

// drainUffd waits for the handler to exit on its own (peer EOF after the FC
// process died), escalating to SIGTERM so a stuck handler cannot block the
// pause path. Either way the process is reaped.
func (a *Agent) drainUffd(sb *sandbox, grace time.Duration) {
	if sb.uffd == nil || sb.uffd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = sb.uffd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = sb.uffd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = sb.uffd.Process.Kill()
			<-done
		}
	}
	sb.uffd = nil
}

// --- small L1 plumbing ------------------------------------------------------

func (a *Agent) putFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return a.l1.PutObject(ctx, key, f, st.Size())
}

// putStream uploads producer output to L1 without a temp file.
func (a *Agent) putStream(ctx context.Context, key string, produce func(io.Writer) error) error {
	return putStreamTo(ctx, a.l1, key, produce)
}

// putStreamTo uploads producer output to an explicit store.
func putStreamTo(ctx context.Context, dst chunkstore.Objects, key string, produce func(io.Writer) error) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := produce(pw)
		pw.CloseWithError(err)
		done <- err
	}()
	putErr := dst.PutObject(ctx, key, pr, -1)
	produceErr := <-done
	if produceErr != nil {
		return produceErr
	}
	return putErr
}

func (a *Agent) receiveObject(ctx context.Context, key string, consume func(io.Reader) error) error {
	rc, err := a.l1.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	return consume(rc)
}

func (a *Agent) fetchFileFrom(ctx context.Context, src chunkstore.Objects, key, path string) error {
	rc, err := src.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, cpErr := io.Copy(f, rc)
	if err := f.Close(); cpErr == nil {
		cpErr = err
	}
	return cpErr
}

// decodeJSONBody decodes one JSON value from a reader.
func decodeJSONBody(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
