package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Tiered reads local-first with a remote (L1) fallback and writes fetched
// chunks through to the local store, so restore faults pay the network price
// once. Put/Delete act on the local tier only — pushing to L1 is the pause
// path's job (Copier).
type Tiered struct {
	Local  Store
	Remote Store
}

func (t Tiered) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	rc, err := t.Local.Get(ctx, hash)
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	remote, err := t.Remote.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer remote.Close()
	data, err := io.ReadAll(remote)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: read remote: %w", hash, err)
	}
	// Write-through is an optimization; serving the chunk must not fail on it.
	_, _ = t.Local.Put(ctx, hash, bytes.NewReader(data), int64(len(data)))
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (t Tiered) Has(ctx context.Context, hash string) (bool, error) {
	ok, err := t.Local.Has(ctx, hash)
	if err != nil || ok {
		return ok, err
	}
	return t.Remote.Has(ctx, hash)
}

func (t Tiered) Put(ctx context.Context, hash string, r io.Reader, size int64) (bool, error) {
	return t.Local.Put(ctx, hash, r, size)
}

func (t Tiered) Delete(ctx context.Context, hash string) error {
	return t.Local.Delete(ctx, hash)
}
