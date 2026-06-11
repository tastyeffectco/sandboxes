package store

import (
	"context"
	"database/sql"
	"time"
)

// Snapshot is the in-memory mirror of a `snapshot` table row
// (migrations/0009). A snapshot is a reusable, frozen copy of a
// sandbox's workspace .img — see ops/design/snapshots-as-templates.md.
type Snapshot struct {
	ID              string
	Name            string
	OwnerToken      string         // auth.Actor.Name — the tenant boundary
	SourceSandboxID sql.NullString // provenance
	CreatedByUserID sql.NullString // untrusted passthrough; provenance only
	BaseImage       string         // recorded, not pinned
	Visibility      string         // 'private' in v1
	Format          string         // 'raw' in v1
	Status          string         // ready | error
	ImagePath       string
	SizeBytes       sql.NullInt64
	ErrorMessage    sql.NullString
	CreatedAt       time.Time
}

const snapshotCols = `id, name, owner_token, source_sandbox_id, created_by_user_id,
	base_image, visibility, format, status, image_path, size_bytes,
	error_message, created_at`

func scanSnapshot(sc interface {
	Scan(dest ...any) error
}) (*Snapshot, error) {
	var s Snapshot
	var createdAt int64
	if err := sc.Scan(
		&s.ID, &s.Name, &s.OwnerToken, &s.SourceSandboxID, &s.CreatedByUserID,
		&s.BaseImage, &s.Visibility, &s.Format, &s.Status, &s.ImagePath,
		&s.SizeBytes, &s.ErrorMessage, &createdAt,
	); err != nil {
		return nil, err
	}
	s.CreatedAt = time.Unix(createdAt, 0).UTC()
	return &s, nil
}

// --- Reads -----------------------------------------------------------

// GetSnapshot returns one snapshot by id, or ErrNotFound.
func (s *Store) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+snapshotCols+` FROM snapshot WHERE id = ?`, id)
	snap, err := scanSnapshot(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return snap, err
}

// ListSnapshotsByOwner returns a tenant's snapshots, newest first.
func (s *Store) ListSnapshotsByOwner(ctx context.Context, ownerToken string) ([]*Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+snapshotCols+` FROM snapshot WHERE owner_token = ? ORDER BY created_at DESC`,
		ownerToken)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Snapshot{}
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// --- Writes (single-writer goroutine) --------------------------------

// CreateSnapshot inserts a snapshot row. Returns ErrConflict on id
// collision.
func (s *Store) CreateSnapshot(ctx context.Context, snap *Snapshot) error {
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now().UTC()
	}
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO snapshot (`+snapshotCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snap.ID, snap.Name, snap.OwnerToken, snap.SourceSandboxID,
			snap.CreatedByUserID, snap.BaseImage, snap.Visibility, snap.Format,
			snap.Status, snap.ImagePath, snap.SizeBytes, snap.ErrorMessage,
			snap.CreatedAt.Unix())
		if err != nil && isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// DeleteSnapshot removes a snapshot row. Returns ErrNotFound if absent.
// The caller is responsible for removing the image file on disk.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `DELETE FROM snapshot WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
