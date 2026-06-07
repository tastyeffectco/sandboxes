package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
)

// Snapshotter is the hourly auto-snapshot goroutine. roadmap §9:
// "For each sandbox with status='stopped' AND last_active_at + 24h <
// now: if a snapshot already exists for today, skip; otherwise take
// one." It is row-driven — orphan workspaces (`.img` with no row) are
// captured only via the manual API, never here.
type Snapshotter struct {
	Mgr      *Manager
	Interval time.Duration // SANDBOXD_SNAPSHOT_INTERVAL_SECONDS (3600); <=0 disables
}

// Run blocks until ctx is cancelled. Interval<=0 disables the loop
// (the rollback lever from roadmap §"Risks").
func (s *Snapshotter) Run(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Mgr.Log.Info("snapshotter: disabled (interval <= 0)")
		return nil
	}
	tk := time.NewTicker(s.Interval)
	defer tk.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
	}
}

func (s *Snapshotter) tick(ctx context.Context) {
	metrics.SnapshotterRuns.Inc()
	idleThreshold := s.Mgr.IdleThreshold
	if idleThreshold <= 0 {
		idleThreshold = 24 * time.Hour
	}
	rows, err := s.Mgr.Store.ListByStatuses(ctx, "stopped")
	if err != nil {
		s.Mgr.Log.Warn("snapshotter: list stopped sandboxes failed", "err", err.Error())
		return
	}
	now := time.Now().UTC()
	cutoff := now.Add(-idleThreshold)
	today := now.Format("2006-01-02")

	for _, sb := range rows {
		// Idle long enough? last_active_at zero-value means "never
		// observed" — treat that as eligible (it has been stopped a
		// while; created_at is older still).
		if !sb.LastActiveAt.IsZero() && sb.LastActiveAt.After(cutoff) {
			continue
		}
		// Already snapshotted today? Skip.
		if existing, err := s.Mgr.List(sb.ID); err == nil {
			already := false
			for _, e := range existing {
				if strings.HasPrefix(e.TS, today) {
					already = true
					break
				}
			}
			if already {
				continue
			}
		}
		if _, err := s.Mgr.Take(ctx, sb.ID, true); err != nil {
			// ErrRunning/ErrNoImg are benign races (the row changed
			// between the list and the Take); log at info.
			s.Mgr.Log.Info("snapshotter: skipped sandbox",
				"sandbox_id", sb.ID, "reason", err.Error())
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// SweepStale removes crash debris under the snapshot tree and the
// workspace tree: half-written `*.img.zst.tmp` snapshots and
// `*.img.bak-*` files left by an interrupted restore. Called by the
// boot reconciler. roadmap §9 "Crash safety" + §10 (the restore
// `.bak` is cleaned on next boot if a restore died mid-flight).
//
// A `.bak` is only swept when the corresponding live `.img` exists —
// otherwise the `.bak` IS the only surviving copy (restore died after
// moving the original aside but before decompressing) and must be
// kept for the operator to recover manually.
func (m *Manager) SweepStale() (tmpRemoved, bakRemoved int) {
	// Stale snapshot .tmp files.
	if entries, err := os.ReadDir(m.SnapshotsRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(m.SnapshotsRoot, e.Name())
			sub, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range sub {
				if strings.HasSuffix(f.Name(), ".img.zst.tmp") {
					if os.Remove(filepath.Join(dir, f.Name())) == nil {
						tmpRemoved++
					}
				}
			}
		}
	}
	// Stale restore .bak files in the workspace tree.
	if entries, err := os.ReadDir(m.WorkspacesRoot); err == nil {
		for _, e := range entries {
			name := e.Name()
			i := strings.Index(name, ".img.bak-")
			if i < 0 {
				continue
			}
			id := name[:i]
			live := filepath.Join(m.WorkspacesRoot, id+".img")
			if _, err := os.Stat(live); err == nil {
				// Live .img is present — the .bak is safe debris.
				if os.Remove(filepath.Join(m.WorkspacesRoot, name)) == nil {
					bakRemoved++
				}
			} else {
				m.Log.Warn("snapshot sweep: keeping restore .bak — live .img missing (interrupted restore; recover manually)",
					"bak", name, "sandbox_id", id)
			}
		}
	}
	return tmpRemoved, bakRemoved
}
