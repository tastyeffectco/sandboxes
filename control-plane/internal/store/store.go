// Package store is the SQLite-backed source of truth for the sandbox
// lifecycle. CLAUDE.md non-negotiable #6: "SQLite is source of truth.
// The reconciler converges Docker to SQLite, never the other way."
//
// Concurrency model (CLAUDE.md control-plane scope): a single writer
// goroutine reads from a buffered channel of write ops; readers use
// `db.QueryRow` / `db.Query` directly. This avoids "database is locked"
// without scattering mutexes across the code.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Sandbox is the in-memory mirror of a `sandbox` table row.
type Sandbox struct {
	ID           string
	Status       string // creating | running | stopped | error
	Image        string
	WorkspaceImg string
	WorkspaceMnt string
	ContainerID  sql.NullString
	CgroupPath   sql.NullString
	MemoryHigh   string
	ErrorMessage sql.NullString
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Ports        []int

	// Phase 5 — activity / lifecycle columns (migrations/0002_activity.sql).
	LastActiveAt   time.Time     // bumped by tailer / poller / exec / wake; zero value if never observed
	StoppedAt      sql.NullInt64 // unix seconds; NULL while running
	KeepaliveUntil sql.NullInt64 // unix seconds; idle reaper skips while > now; NULL = no override

	// Phase 6 — container bridge IP for egress correlation
	// (migrations/0003_container_ip.sql). NULL while stopped.
	ContainerIP sql.NullString

	// idle_policy controls the idle reaper's behaviour for this sandbox
	// (migrations/0011_idle_policy.sql). 'sleep' (default) = idle-stop +
	// wake-on-request; 'always_on' = never idle-stopped.
	IdlePolicy string

	// Phase 8 — external identity passthrough + visibility
	// (migrations/0004_external_identity.sql). The external_* columns
	// are opaque upstream identifiers sandboxd never interprets beyond
	// equality checks; NULL only on legacy rows until backfill-legacy
	// runs. Visibility is 'public' | 'private'.
	ExternalUserID      sql.NullString
	ExternalProjectID   sql.NullString
	ExternalWorkspaceID sql.NullString
	Visibility          string
}

// WorkspaceOwner is the durable sandbox_id -> upstream-identity
// binding (migrations/0004). It survives DELETE /sandbox/{id}; only
// per-sandbox purge removes it.
type WorkspaceOwner struct {
	SandboxID           string
	ExternalUserID      string
	ExternalProjectID   sql.NullString
	ExternalWorkspaceID sql.NullString
	CreatedAt           time.Time
}

// Store wraps an open *sql.DB plus the write-loop goroutine.
type Store struct {
	db      *sql.DB
	writes  chan writeOp
	doneCh  chan struct{}
	closeCh chan struct{}
}

// Open opens the database at dsn, applies migrations, and starts the
// single-writer goroutine. The caller MUST call Close to drain pending
// writes before exiting.
func Open(ctx context.Context, dsn string, migrationsDir string) (*Store, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite handles one connection at a time at the file level; the
	// writer goroutine serializes writes. Allow a small pool for reads.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := Migrate(ctx, db, migrationsDir); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{
		db:      db,
		writes:  make(chan writeOp, 64),
		doneCh:  make(chan struct{}),
		closeCh: make(chan struct{}),
	}
	go s.writeLoop()
	return s, nil
}

// Close drains the write channel and closes the database.
func (s *Store) Close() error {
	close(s.closeCh)
	<-s.doneCh
	return s.db.Close()
}

// DB exposes the underlying *sql.DB for read-only queries.
// Writes MUST go through the write methods below so they're serialised
// by the single writer goroutine.
func (s *Store) DB() *sql.DB { return s.db }

// ErrNotFound is returned when a row lookup yields zero rows.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a write would violate uniqueness
// (typically: POST /sandbox with an id that already has a row).
var ErrConflict = errors.New("conflict")

// --- Reads -----------------------------------------------------------

func (s *Store) Get(ctx context.Context, id string) (*Sandbox, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, status, image, workspace_img, workspace_mnt,
		       container_id, cgroup_path, memory_high, error_message,
		       created_at, updated_at,
		       last_active_at, stopped_at, keepalive_until,
		       container_ip,
		       external_user_id, external_project_id, external_workspace_id, visibility,
		       idle_policy
		  FROM sandbox WHERE id = ?`, id)
	sb, err := scanSandbox(row)
	if err != nil {
		return nil, err
	}
	ports, err := s.portsFor(ctx, id)
	if err != nil {
		return nil, err
	}
	sb.Ports = ports
	return sb, nil
}

// GitRemote returns the sandbox's configured git push target, or "" if
// none is set. A dedicated single-column read so the common SELECT path
// and scanSandbox stay untouched.
func (s *Store) GitRemote(ctx context.Context, id string) (string, error) {
	var url sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT git_remote_url FROM sandbox WHERE id = ?`, id).Scan(&url)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return url.String, nil
}

func (s *Store) List(ctx context.Context) ([]*Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status, image, workspace_img, workspace_mnt,
		       container_id, cgroup_path, memory_high, error_message,
		       created_at, updated_at,
		       last_active_at, stopped_at, keepalive_until,
		       container_ip,
		       external_user_id, external_project_id, external_workspace_id, visibility,
		       idle_policy
		  FROM sandbox ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	for _, sb := range out {
		ports, err := s.portsFor(ctx, sb.ID)
		if err != nil {
			return nil, err
		}
		sb.Ports = ports
	}
	return out, nil
}

// ListByStatuses returns rows whose status is in the given set.
// Used by the reconciler.
func (s *Store) ListByStatuses(ctx context.Context, statuses ...string) ([]*Sandbox, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	q := `SELECT id, status, image, workspace_img, workspace_mnt,
	             container_id, cgroup_path, memory_high, error_message,
	             created_at, updated_at,
	             last_active_at, stopped_at, keepalive_until,
	             container_ip,
	             external_user_id, external_project_id, external_workspace_id, visibility,
	             idle_policy
	        FROM sandbox WHERE status IN (?`
	args := make([]any, 0, len(statuses))
	args = append(args, statuses[0])
	for _, st := range statuses[1:] {
		q += ",?"
		args = append(args, st)
	}
	q += ")"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	for _, sb := range out {
		ports, err := s.portsFor(ctx, sb.ID)
		if err != nil {
			return nil, err
		}
		sb.Ports = ports
	}
	return out, nil
}

// ListIdleCandidates returns rows whose status='running' AND
// last_active_at < cutoff, ordered by last_active_at ascending
// (oldest first). The idle reaper uses this; it must still apply
// the inflight-exec / keepalive / open-connection skip rules in
// memory before calling docker stop.
func (s *Store) ListIdleCandidates(ctx context.Context, cutoff time.Time) ([]*Sandbox, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, status, image, workspace_img, workspace_mnt,
		       container_id, cgroup_path, memory_high, error_message,
		       created_at, updated_at,
		       last_active_at, stopped_at, keepalive_until,
		       container_ip,
		       external_user_id, external_project_id, external_workspace_id, visibility,
		       idle_policy
		  FROM sandbox
		 WHERE status='running' AND last_active_at < ? AND idle_policy != 'always_on'
		 ORDER BY last_active_at ASC`, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	// Ports aren't needed for the idle decision; skip the per-row
	// lookup to keep this hot path cheap.
	return out, nil
}

func (s *Store) portsFor(ctx context.Context, id string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM sandbox_port WHERE sandbox_id = ? ORDER BY port`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ports []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		ports = append(ports, p)
	}
	return ports, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSandbox(s scanner) (*Sandbox, error) {
	sb := &Sandbox{}
	var createdUnix, updatedUnix, lastActiveUnix int64
	err := s.Scan(
		&sb.ID, &sb.Status, &sb.Image, &sb.WorkspaceImg, &sb.WorkspaceMnt,
		&sb.ContainerID, &sb.CgroupPath, &sb.MemoryHigh, &sb.ErrorMessage,
		&createdUnix, &updatedUnix,
		&lastActiveUnix, &sb.StoppedAt, &sb.KeepaliveUntil,
		&sb.ContainerIP,
		&sb.ExternalUserID, &sb.ExternalProjectID, &sb.ExternalWorkspaceID, &sb.Visibility,
		&sb.IdlePolicy,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sb.CreatedAt = time.Unix(createdUnix, 0).UTC()
	sb.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
	if lastActiveUnix > 0 {
		sb.LastActiveAt = time.Unix(lastActiveUnix, 0).UTC()
	}
	if sb.IdlePolicy == "" {
		sb.IdlePolicy = "sleep"
	}
	return sb, nil
}

// --- Phase 8 reads: external-identity filtering + workspace_owner ----

const sandboxSelectCols = `id, status, image, workspace_img, workspace_mnt,
	       container_id, cgroup_path, memory_high, error_message,
	       created_at, updated_at,
	       last_active_at, stopped_at, keepalive_until,
	       container_ip,
	       external_user_id, external_project_id, external_workspace_id, visibility,
	       idle_policy`

// ListFiltered returns sandbox rows filtered by external_user_id and/
// or external_project_id. An empty string for either filter means "do
// not constrain on this column". Used by GET /sandboxes?external_*.
func (s *Store) ListFiltered(ctx context.Context, externalUserID, externalProjectID string) ([]*Sandbox, error) {
	q := `SELECT ` + sandboxSelectCols + ` FROM sandbox WHERE 1=1`
	args := []any{}
	if externalUserID != "" {
		q += ` AND external_user_id = ?`
		args = append(args, externalUserID)
	}
	if externalProjectID != "" {
		q += ` AND external_project_id = ?`
		args = append(args, externalProjectID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	for _, sb := range out {
		ports, err := s.portsFor(ctx, sb.ID)
		if err != nil {
			return nil, err
		}
		sb.Ports = ports
	}
	return out, nil
}

func scanWorkspaceOwner(sc scanner) (*WorkspaceOwner, error) {
	wo := &WorkspaceOwner{}
	var createdUnix int64
	err := sc.Scan(&wo.SandboxID, &wo.ExternalUserID,
		&wo.ExternalProjectID, &wo.ExternalWorkspaceID, &createdUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	wo.CreatedAt = time.Unix(createdUnix, 0).UTC()
	return wo, nil
}

// GetWorkspaceOwner returns the durable ownership binding for a
// sandbox id, or ErrNotFound. Survives DELETE /sandbox/{id}.
func (s *Store) GetWorkspaceOwner(ctx context.Context, sandboxID string) (*WorkspaceOwner, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT sandbox_id, external_user_id, external_project_id,
		       external_workspace_id, created_at
		  FROM workspace_owner WHERE sandbox_id = ?`, sandboxID)
	return scanWorkspaceOwner(row)
}

// ListAllWorkspaceOwners returns every workspace_owner row. Used by
// the reconciler's periodic orphan check (Phase 8 step 13).
func (s *Store) ListAllWorkspaceOwners(ctx context.Context) ([]*WorkspaceOwner, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sandbox_id, external_user_id, external_project_id,
		       external_workspace_id, created_at
		  FROM workspace_owner`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorkspaceOwner
	for rows.Next() {
		wo, err := scanWorkspaceOwner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wo)
	}
	return out, nil
}

// WorkspaceOwnerSandboxIDs returns the sandbox ids owned by an
// external user (scope="user") or external project (scope="project").
// Used by the per-external-user / per-external-project purge.
func (s *Store) WorkspaceOwnerSandboxIDs(ctx context.Context, scope, value string) ([]string, error) {
	col := "external_user_id"
	if scope == "project" {
		col = "external_project_id"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT sandbox_id FROM workspace_owner WHERE `+col+` = ?`, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CountSandboxesMissingExternalUser returns how many running rows have
// no external_user_id — the reconciler logs (does not destroy) these.
func (s *Store) CountSandboxesMissingExternalUser(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sandbox WHERE status='running' AND external_user_id IS NULL`).Scan(&n)
	return n, err
}
