package snapshot

import (
	"os"
	"time"
)

// Prune enforces snapshot retention for one id. It acquires the per-id
// lock; takeLocked calls pruneLocked directly (it already holds it).
//
// roadmap §9 "Pruning" + the keep-one invariant:
//   - delete snapshots older than RetentionDays,
//   - BUT never delete the last surviving snapshot for an id, even if
//     it is past the window. Purge of long-archived workspaces is a
//     Phase 8 operation, not a side effect of retention.
func (m *Manager) Prune(id string) error {
	m.Locks.Lock(id)
	defer m.Locks.Unlock(id)
	return m.pruneLocked(id)
}

func (m *Manager) pruneLocked(id string) error {
	retentionDays := m.RetentionDays
	if retentionDays <= 0 {
		retentionDays = 7
	}
	tss, err := m.listTimestamps(id)
	if err != nil {
		return err
	}
	// Fewer than two snapshots: nothing to prune (keep-one invariant
	// means a single snapshot is always kept).
	if len(tss) <= 1 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	// tss is newest-first. The newest snapshot is always kept (it is
	// both within-window in the common case AND the keep-one
	// guarantee). Walk the rest; delete those older than the cutoff.
	// Because the newest is retained unconditionally, the invariant
	// "at least one always survives" holds even if every snapshot is
	// past the window.
	for _, ts := range tss[1:] {
		t, err := time.Parse(tsLayout, ts)
		if err != nil {
			// Unparseable name — leave it, surfacing rather than
			// silently deleting something we don't understand.
			m.Log.Warn("snapshot prune: unparseable timestamp; skipping",
				"sandbox_id", id, "ts", ts)
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(m.zstPath(id, ts))
			_ = os.Remove(m.metaPath(id, ts))
			m.Log.Info("snapshot prune: deleted past-retention snapshot",
				"sandbox_id", id, "ts", ts, "retention_days", retentionDays)
		}
	}
	return nil
}
