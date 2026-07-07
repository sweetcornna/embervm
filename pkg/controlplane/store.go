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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

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

// Migrate applies the embedded schema (idempotent — the SQL uses IF NOT
// EXISTS, so M1 needs no migration-version table).
func (s *Store) Migrate(ctx context.Context) error {
	sqlBytes, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("apply migration: %w", err)
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
	err := s.pool.QueryRow(ctx,
		`INSERT INTO sandboxes (id,template_id,state,vcpus,memory_mib,data_disk_gib,owner)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 RETURNING id,template_id,state,vcpus,memory_mib,data_disk_gib,netns,owner,error,created_at,updated_at,paused_at`,
		sb.ID, sb.TemplateID, sb.State, sb.VCPUs, sb.MemoryMiB, sb.DataDiskGiB, sb.Owner).
		Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
			&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt)
	return sb, err
}

func scanSandbox(row pgx.Row) (Sandbox, error) {
	var sb Sandbox
	err := row.Scan(&sb.ID, &sb.TemplateID, &sb.State, &sb.VCPUs, &sb.MemoryMiB, &sb.DataDiskGiB,
		&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

const sandboxCols = `id,template_id,state,vcpus,memory_mib,data_disk_gib,netns,owner,error,created_at,updated_at,paused_at`

// GetSandbox fetches a sandbox by id.
func (s *Store) GetSandbox(ctx context.Context, id string) (Sandbox, error) {
	return scanSandbox(s.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1`, id))
}

// ListSandboxes returns sandboxes, optionally filtered by state, newest first.
func (s *Store) ListSandboxes(ctx context.Context, state string) ([]Sandbox, error) {
	q := `SELECT ` + sandboxCols + ` FROM sandboxes`
	args := []any{}
	if state != "" {
		q += ` WHERE state=$1`
		args = append(args, state)
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
			&sb.Netns, &sb.Owner, &sb.Error, &sb.CreatedAt, &sb.UpdatedAt, &sb.PausedAt); err != nil {
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
