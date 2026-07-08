// Package controlplane is the EmberVM API server: a PostgreSQL-backed store,
// bearer-token auth with per-token quota, and a Gin REST surface that drives
// a nodeapi.Agent through the sandbox lifecycle. PostgreSQL is the single
// source of truth (docs/zh/04 §6); Redis and the Gateway are M4.
package controlplane

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict marks a compare-and-swap state change that lost its race.
var ErrConflict = errors.New("state changed concurrently")

// Template is a persisted template row.
type Template struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Image     string     `json:"image"`
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
}

// Sandbox is a persisted sandbox row.
type Sandbox struct {
	ID          string     `json:"id"`
	TemplateID  string     `json:"template_id"`
	State       string     `json:"state"`
	VCPUs       int        `json:"vcpus"`
	MemoryMiB   int        `json:"memory_mib"`
	DataDiskGiB int        `json:"data_disk_gib"`
	Netns       string     `json:"netns,omitempty"`
	Owner       string     `json:"owner,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PausedAt    *time.Time `json:"paused_at,omitempty"`
	// ArtifactPaths are the guest paths preserved when the sandbox is
	// RECYCLED (M3 selective restore); empty means keep nothing.
	ArtifactPaths []string   `json:"artifact_paths,omitempty"`
	PrewarmedAt   *time.Time `json:"prewarmed_at,omitempty"`
	// NodeID is where the sandbox currently lives (M4 placement).
	NodeID string `json:"node_id,omitempty"`
}

// Store is the PostgreSQL-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore connects a pool to databaseURL.
func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Migrate applies every embedded migration in filename order (idempotent —
// the SQL uses IF NOT EXISTS everywhere, so no version table is needed yet).
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// --- templates --------------------------------------------------------------

// CreateTemplate inserts a template in BUILDING state.
func (s *Store) CreateTemplate(ctx context.Context, id, name, image string) (Template, error) {
	var t Template
	err := s.pool.QueryRow(ctx,
		`INSERT INTO templates (id, name, image, state) VALUES ($1,$2,$3,'BUILDING')
		 RETURNING id,name,image,state,error,created_at,ready_at`,
		id, name, image).Scan(&t.ID, &t.Name, &t.Image, &t.State, &t.Error, &t.CreatedAt, &t.ReadyAt)
	return t, err
}

// SetTemplateState updates a template's state (and ready_at when READY, or
// error when ERROR).
func (s *Store) SetTemplateState(ctx context.Context, id, state, errMsg string) error {
	var readyAt *time.Time
	if state == "READY" {
		now := time.Now()
		readyAt = &now
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE templates SET state=$2, error=$3, ready_at=COALESCE($4, ready_at) WHERE id=$1`,
		id, state, errMsg, readyAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanTemplate(row pgx.Row) (Template, error) {
	var t Template
	err := row.Scan(&t.ID, &t.Name, &t.Image, &t.State, &t.Error, &t.CreatedAt, &t.ReadyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	return t, err
}

// GetTemplate fetches a template by id.
func (s *Store) GetTemplate(ctx context.Context, id string) (Template, error) {
	return scanTemplate(s.pool.QueryRow(ctx,
		`SELECT id,name,image,state,error,created_at,ready_at FROM templates WHERE id=$1`, id))
}

// ListTemplates returns all templates, newest first.
func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id,name,image,state,error,created_at,ready_at FROM templates ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Image, &t.State, &t.Error, &t.CreatedAt, &t.ReadyAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTemplate removes a template.
func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM templates WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- sandboxes --------------------------------------------------------------

// CreateSandbox inserts a sandbox in the given initial state.
func (s *Store) CreateSandbox(ctx context.Context, sb Sandbox) (Sandbox, error) {
	if sb.ArtifactPaths == nil {
		sb.ArtifactPaths = []string{}
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sandboxes (id,template_id,state,vcpus,memory_mib,data_disk_gib,owner,artifact_paths)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING `+sandboxCols,
		sb.ID, sb.TemplateID, sb.State, sb.VCPUs, sb.MemoryMiB, sb.DataDiskGiB, sb.Owner, sb.ArtifactPaths).
		Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
			&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt,
			&sb.ArtifactPaths, &sb.PrewarmedAt, &sb.NodeID)
	return sb, err
}

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var sb Sandbox
	err := row.Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
		&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt,
		&sb.ArtifactPaths, &sb.PrewarmedAt, &sb.NodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

const sandboxCols = `id,template_id,state,vcpus,memory_mib,data_disk_gib,netns,owner,error,created_at,updated_at,paused_at,artifact_paths,prewarmed_at,node_id`

// GetSandbox fetches a sandbox by id.
func (s *Store) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	return scanSandbox(s.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1`, id))
}

// ListSandboxes returns sandboxes, optionally filtered by owner and/or state,
// newest first. Empty owner or state means no filter on that column; the API
// server always passes the authenticated owner so tenants never see each
// other's sandboxes.
func (s *Store) ListSandboxes(ctx context.Context, owner, state string) ([]Sandbox, error) {
	q := `SELECT ` + sandboxCols + ` FROM sandboxes`
	args := []any{}
	var conds []string
	if owner != "" {
		args = append(args, owner)
		conds = append(conds, `owner=$`+strconv.Itoa(len(args)))
	}
	if state != "" {
		args = append(args, state)
		conds = append(conds, `state=$`+strconv.Itoa(len(args)))
	}
	for i, cond := range conds {
		if i == 0 {
			q += ` WHERE `
		} else {
			q += ` AND `
		}
		q += cond
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		if err := rows.Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
			&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt,
			&sb.ArtifactPaths, &sb.PrewarmedAt, &sb.NodeID); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// SetSandboxState updates a sandbox's state, stamps updated_at (and paused_at
// when entering PAUSED_HOT), and appends a sandbox_events row — atomically.
func (s *Store) SetSandboxState(ctx context.Context, id, from, to, netns, errMsg string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var pausedAt *time.Time
	if to == "PAUSED_HOT" {
		now := time.Now()
		pausedAt = &now
	}
	tag, err := tx.Exec(ctx,
		`UPDATE sandboxes SET state=$2, error=$3,
		   netns=COALESCE(NULLIF($4,''), netns),
		   paused_at=COALESCE($5, paused_at),
		   prewarmed_at=CASE WHEN $2='PAUSED_HOT' THEN NULL ELSE prewarmed_at END,
		   updated_at=now()
		 WHERE id=$1`,
		id, to, errMsg, netns, pausedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sandbox_events (sandbox_id, from_state, to_state) VALUES ($1,$2,$3)`,
		id, from, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TransitionSandbox is the compare-and-swap state change the lifecycle
// engine uses: it only applies when the row is still in `from`, so a resume
// racing a TTL transition loses cleanly (ErrConflict) instead of clobbering.
func (s *Store) TransitionSandbox(ctx context.Context, id, from, to, errMsg string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		`UPDATE sandboxes SET state=$3, error=$4,
		   prewarmed_at=CASE WHEN $3='PAUSED_HOT' THEN NULL ELSE prewarmed_at END,
		   updated_at=now()
		 WHERE id=$1 AND state=$2`,
		id, from, to, errMsg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO sandbox_events (sandbox_id, from_state, to_state) VALUES ($1,$2,$3)`,
		id, from, to); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListTransitionDue returns sandboxes sitting in `state` since before
// `before` (updated_at is stamped on every transition, so it marks when the
// current state was entered).
func (s *Store) ListTransitionDue(ctx context.Context, state string, before time.Time) ([]Sandbox, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE state=$1 AND updated_at < $2 ORDER BY updated_at`,
		state, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		if err := rows.Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
			&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt,
			&sb.ArtifactPaths, &sb.PrewarmedAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// WakeIntervals returns the durations between each pause and the following
// resume (oldest first) — the pre-warm predictor's input.
func (s *Store) WakeIntervals(ctx context.Context, id string) ([]time.Duration, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT to_state, at FROM sandbox_events
		 WHERE sandbox_id=$1 AND to_state IN ('PAUSED_HOT','RESUMING') ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Duration
	var pausedAt *time.Time
	for rows.Next() {
		var state string
		var at time.Time
		if err := rows.Scan(&state, &at); err != nil {
			return nil, err
		}
		switch state {
		case "PAUSED_HOT":
			t := at
			pausedAt = &t
		case "RESUMING":
			if pausedAt != nil {
				out = append(out, at.Sub(*pausedAt))
				pausedAt = nil
			}
		}
	}
	return out, rows.Err()
}

// SetPrewarmedAt stamps the last pre-warm pull (nil clears it, e.g. on pause).
func (s *Store) SetPrewarmedAt(ctx context.Context, id string, at *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sandboxes SET prewarmed_at=$2 WHERE id=$1`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetSandboxNode records placement.
func (s *Store) SetSandboxNode(ctx context.Context, id, nodeID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sandboxes SET node_id=$2, updated_at=now() WHERE id=$1`, id, nodeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Node is a registered worker (M4 static membership).
type Node struct {
	ID          string    `json:"id"`
	Addr        string    `json:"addr,omitempty"`
	State       string    `json:"state"`
	CapacityMiB int       `json:"capacity_mib"`
	LastSeen    time.Time `json:"last_seen"`
}

// UpsertNode registers/refreshes a node.
func (s *Store) UpsertNode(ctx context.Context, n Node) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO nodes (id, addr, state, capacity_mib, last_seen)
		 VALUES ($1,$2,'up',$3,now())
		 ON CONFLICT (id) DO UPDATE SET addr=$2, capacity_mib=$3`,
		n.ID, n.Addr, n.CapacityMiB)
	return err
}

// TouchNode records a successful health poll.
func (s *Store) TouchNode(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE nodes SET last_seen=now(), state='up' WHERE id=$1`, id)
	return err
}

// SetNodeState flips a node up/down.
func (s *Store) SetNodeState(ctx context.Context, id, state string) error {
	_, err := s.pool.Exec(ctx, `UPDATE nodes SET state=$2 WHERE id=$1`, id, state)
	return err
}

// ListNodes returns the registry.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, addr, state, capacity_mib, last_seen FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Addr, &n.State, &n.CapacityMiB, &n.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeUsage sums the memory of active sandboxes per node (the bin-packing
// constraint; PostgreSQL is the single source of truth for placement).
func (s *Store) NodeUsage(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT node_id, COALESCE(SUM(memory_mib),0) FROM sandboxes
		 WHERE state IN ('STARTING','RUNNING','RESUMING','PAUSING') AND node_id <> ''
		 GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var used int
		if err := rows.Scan(&id, &used); err != nil {
			return nil, err
		}
		out[id] = used
	}
	return out, rows.Err()
}

// FailRunningOnNode marks a dead node's active sandboxes FAILED (their last
// write-through snapshot remains restorable elsewhere).
func (s *Store) FailRunningOnNode(ctx context.Context, nodeID, reason string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sandboxes SET state='FAILED', error=$2, updated_at=now()
		 WHERE node_id=$1 AND state IN ('STARTING','RUNNING','RESUMING','PAUSING')`,
		nodeID, reason)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteSandbox removes a sandbox row.
func (s *Store) DeleteSandbox(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sandboxes WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveSandboxes counts an owner's sandboxes not in a terminal state
// (STOPPED/FAILED) — the quota denominator.
func (s *Store) CountActiveSandboxes(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM sandboxes WHERE owner=$1 AND state NOT IN ('STOPPED','FAILED')`,
		owner).Scan(&n)
	return n, err
}

// CountSandboxEvents returns how many events a sandbox has (test/support).
func (s *Store) CountSandboxEvents(ctx context.Context, id string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_events WHERE sandbox_id=$1`, id).Scan(&n)
	return n, err
}
