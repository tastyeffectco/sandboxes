package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// writeOp is the union of all write requests sent to the single
// writer goroutine.
type writeOp struct {
	fn   func(*sql.DB) error // closure that performs the SQL
	done chan error          // closed by the writer after fn returns
}

// writeLoop is the single writer goroutine. It serializes all writes
// to the SQLite file, eliminating "database is locked" errors that
// arise from concurrent writers in WAL mode.
func (s *Store) writeLoop() {
	defer close(s.doneCh)
	for {
		select {
		case op := <-s.writes:
			op.done <- op.fn(s.db)
			close(op.done)
		case <-s.closeCh:
			// Drain any pending ops before exit.
			for {
				select {
				case op := <-s.writes:
					op.done <- op.fn(s.db)
					close(op.done)
				default:
					return
				}
			}
		}
	}
}

// submit posts a write op to the writer goroutine and waits for it.
func (s *Store) submit(ctx context.Context, fn func(*sql.DB) error) error {
	op := writeOp{fn: fn, done: make(chan error, 1)}
	select {
	case s.writes <- op:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-op.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// --- Public write methods --------------------------------------------

// Create inserts a new sandbox row + its ports atomically. Phase 8:
// also writes the external-identity columns + visibility, and inserts
// the durable workspace_owner binding (idempotent via ON CONFLICT —
// the supported id-reuse path reattaches an existing .img whose
// workspace_owner row the API layer has already validated still
// matches the caller's external.user_id).
// Returns ErrConflict if a sandbox row with the same id already exists.
func (s *Store) Create(ctx context.Context, sb *Sandbox) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := time.Now().Unix()
		visibility := sb.Visibility
		if visibility == "" {
			visibility = "public"
		}
		idlePolicy := sb.IdlePolicy
		if idlePolicy == "" {
			idlePolicy = "sleep"
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sandbox (id, status, image, workspace_img, workspace_mnt,
			                    container_id, cgroup_path, memory_high, error_message,
			                    created_at, updated_at,
			                    external_user_id, external_project_id,
			                    external_workspace_id, visibility,
			                    idle_policy)
			VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, NULL, ?, ?, ?, ?, ?, ?, ?)`,
			sb.ID, sb.Status, sb.Image, sb.WorkspaceImg, sb.WorkspaceMnt,
			sb.MemoryHigh, now, now,
			sb.ExternalUserID, sb.ExternalProjectID, sb.ExternalWorkspaceID, visibility, idlePolicy)

		if err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		for _, p := range sb.Ports {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO sandbox_port (sandbox_id, port) VALUES (?, ?)`,
				sb.ID, p)
			if err != nil {
				return err
			}
		}
		if sb.ExternalUserID.Valid {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO workspace_owner
				    (sandbox_id, external_user_id, external_project_id,
				     external_workspace_id, created_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(sandbox_id) DO NOTHING`,
				sb.ID, sb.ExternalUserID.String, sb.ExternalProjectID,
				sb.ExternalWorkspaceID, now)
			if err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// Claim updates the external identity of a (typically legacy) sandbox
// on both the sandbox row and the durable workspace_owner row, in one
// transaction. project / workspace are optional ("" leaves them as
// they were on the workspace_owner row, and sets the sandbox column
// to that same value). Returns ErrNotFound if no sandbox row exists.
func (s *Store) Claim(ctx context.Context, sandboxID, externalUserID, externalProjectID, externalWorkspaceID string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now := time.Now().Unix()
		projectArg := nullIfEmpty(externalProjectID)
		workspaceArg := nullIfEmpty(externalWorkspaceID)
		res, err := tx.ExecContext(ctx, `
			UPDATE sandbox
			   SET external_user_id      = ?,
			       external_project_id   = ?,
			       external_workspace_id = ?,
			       updated_at            = ?
			 WHERE id = ?`,
			externalUserID, projectArg, workspaceArg, now, sandboxID)
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
		// Upsert the workspace_owner row so a claim also repairs a
		// legacy sandbox that never had one.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO workspace_owner
			    (sandbox_id, external_user_id, external_project_id,
			     external_workspace_id, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(sandbox_id) DO UPDATE SET
			    external_user_id      = excluded.external_user_id,
			    external_project_id   = excluded.external_project_id,
			    external_workspace_id = excluded.external_workspace_id`,
			sandboxID, externalUserID, projectArg, workspaceArg, now)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
}

// PurgeSandbox removes the sandbox row AND the durable workspace_owner
// row for an id, in one transaction. Ports cascade via the FK. This is
// the only path that deletes a workspace_owner row. The caller is
// responsible for the container / loopback / .img / snapshot teardown
// before calling this.
func (s *Store) PurgeSandbox(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_owner WHERE sandbox_id=?`, id); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// InsertAudit appends one append-only audit_log row. detail is a
// pre-encoded JSON string ("" allowed). Best-effort callers ignore
// the error after logging it; the audit package wraps this.
func (s *Store) InsertAudit(ctx context.Context, at int64, actorKind, actorName, actorIP, externalUserID, action, target, detail string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO audit_log
			    (at, actor_kind, actor_name, actor_ip,
			     external_user_id, action, target, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			at, actorKind, nullIfEmpty(actorName), nullIfEmpty(actorIP),
			nullIfEmpty(externalUserID), action, nullIfEmpty(target), nullIfEmpty(detail))
		return err
	})
}

// BackfillLegacySandboxes sets external_user_id = sentinel on every
// sandbox row where it is currently NULL. Idempotent — a second run
// affects zero rows. Returns the number of rows updated.
func (s *Store) BackfillLegacySandboxes(ctx context.Context, sentinel string) (int, error) {
	var n int
	err := s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET external_user_id = ?, updated_at = ?
			 WHERE external_user_id IS NULL`, sentinel, time.Now().Unix())
		if err != nil {
			return err
		}
		c, err := res.RowsAffected()
		if err == nil {
			n = int(c)
		}
		return nil
	})
	return n, err
}

// EnsureWorkspaceOwner inserts a workspace_owner row for sandboxID if
// one does not already exist. Returns true when a row was inserted.
// Used by `sandboxd backfill-legacy` to give every on-disk .img a
// durable ownership binding.
func (s *Store) EnsureWorkspaceOwner(ctx context.Context, sandboxID, externalUserID string) (bool, error) {
	var inserted bool
	err := s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `
			INSERT INTO workspace_owner
			    (sandbox_id, external_user_id, external_project_id,
			     external_workspace_id, created_at)
			VALUES (?, ?, NULL, NULL, ?)
			ON CONFLICT(sandbox_id) DO NOTHING`,
			sandboxID, externalUserID, time.Now().Unix())
		if err != nil {
			return err
		}
		c, err := res.RowsAffected()
		if err == nil {
			inserted = c > 0
		}
		return nil
	})
	return inserted, err
}

// SetGitRemote sets (or clears, with "") the per-sandbox git push
// target. Called once on create when the caller supplies a remote.
func (s *Store) SetGitRemote(ctx context.Context, id, url string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE sandbox SET git_remote_url = ?, updated_at = ? WHERE id = ?`,
			nullIfEmpty(url), time.Now().Unix(), id)
		return err
	})
}

// nullIfEmpty maps "" to a NULL-valued NullString so empty optional
// columns store as NULL rather than the empty string.
func nullIfEmpty(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// MarkRunning transitions a sandbox to status='running' and records
// container_id + cgroup_path. Clears error_message.
func (s *Store) MarkRunning(ctx context.Context, id, containerID, cgroupPath string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		now := time.Now().Unix()
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET status='running', container_id=?, cgroup_path=?,
			       error_message=NULL, updated_at=?
			 WHERE id=?`, containerID, cgroupPath, now, id)
		return err
	})
}

// MarkStopped is used by the reconciler when a container has gone away
// but its row remains. container_id and cgroup_path are preserved as
// last-known.
func (s *Store) MarkStopped(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE sandbox SET status='stopped', updated_at=? WHERE id=?`,
			time.Now().Unix(), id)
		return err
	})
}

// MarkError records a failure on the row. Used by both the create
// flow's failure cleanup and the reconciler's error path.
func (s *Store) MarkError(ctx context.Context, id, msg string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE sandbox SET status='error', error_message=?, updated_at=? WHERE id=?`,
			msg, time.Now().Unix(), id)
		return err
	})
}

// BumpLastActive sets last_active_at = max(current, t) for the given
// id. Called by the access-log tailer, open-connection poller, exec
// enter/exit, and wake handler. Idempotent — moves the timestamp
// forward only.
func (s *Store) BumpLastActive(ctx context.Context, id string, t time.Time) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET last_active_at = MAX(last_active_at, ?),
			       updated_at     = ?
			 WHERE id = ?`, t.Unix(), time.Now().Unix(), id)
		return err
	})
}

// MarkStoppedAt transitions a sandbox to status='stopped' AND records
// stopped_at = now. Used by the idle reaper and pressure reaper. The
// container_id and cgroup_path are deliberately preserved for the
// audit trail; the reconciler ignores them on running checks.
func (s *Store) MarkStoppedAt(ctx context.Context, id string, t time.Time) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET status     = 'stopped',
			       stopped_at = ?,
			       updated_at = ?
			 WHERE id = ?`, t.Unix(), t.Unix(), id)
		return err
	})
}

// MarkRunningWoke is like MarkRunning but clears stopped_at and bumps
// last_active_at — used by the wake handler when a stopped sandbox
// comes back up.
func (s *Store) MarkRunningWoke(ctx context.Context, id, containerID, cgroupPath string, t time.Time) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET status='running',
			       container_id=?,
			       cgroup_path=?,
			       error_message=NULL,
			       stopped_at=NULL,
			       last_active_at=?,
			       updated_at=?
			 WHERE id=?`,
			containerID, cgroupPath, t.Unix(), t.Unix(), id)
		return err
	})
}

// SetKeepaliveUntil sets keepalive_until for the given id. The caller
// is expected to have already clamped the value to the maximum
// permitted offset; the store stores whatever it's given.
func (s *Store) SetKeepaliveUntil(ctx context.Context, id string, until time.Time) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox SET keepalive_until = ?, updated_at = ? WHERE id = ?`,
			until.Unix(), time.Now().Unix(), id)
		return err
	})
}

// SetContainerIP records the container's bridge IP (Phase 6). Called
// by handleCreate after `docker run` returns and by the wake handler
// after `docker start`. Idempotent — overwriting with the same value
// is a no-op.
func (s *Store) SetContainerIP(ctx context.Context, id, ip string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox SET container_ip = ?, updated_at = ? WHERE id = ?`,
			ip, time.Now().Unix(), id)
		return err
	})
}

// ClearContainerIP NULLs out container_ip. Called by handleDelete and
// by the idle / pressure reapers after `docker stop` succeeds — the
// stopped container's old bridge IP is no longer ours to advertise to
// nftables.
func (s *Store) ClearContainerIP(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			UPDATE sandbox SET container_ip = NULL, updated_at = ? WHERE id = ?`,
			time.Now().Unix(), id)
		return err
	})
}

// BackfillRunningActivity sets last_active_at = updated_at on every
// running row that still has the legacy default of 0. Called once at
// startup so the idle reaper doesn't immediately stop everything
// when migration 0002 runs against a populated DB.
func (s *Store) BackfillRunningActivity(ctx context.Context) (int, error) {
	var n int
	err := s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `
			UPDATE sandbox
			   SET last_active_at = updated_at
			 WHERE status = 'running' AND last_active_at = 0`)
		if err != nil {
			return err
		}
		c, err := res.RowsAffected()
		if err == nil {
			n = int(c)
		}
		return nil
	})
	return n, err
}

// Delete removes the sandbox row. Ports cascade via the FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `DELETE FROM sandbox WHERE id=?`, id)
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

// isUniqueViolation returns true for SQLite's "UNIQUE constraint
// failed" errors. We deliberately compare on substring instead of an
// error code constant to keep the dependency on the driver internals
// minimal.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, errUnique) || // overridable in tests
		containsAny(err.Error(), "UNIQUE constraint failed", "constraint failed: UNIQUE")
}

var errUnique = errors.New("UNIQUE constraint failed")

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

// tiny stdlib-free strings.Contains for keeping this file dep-light.
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
