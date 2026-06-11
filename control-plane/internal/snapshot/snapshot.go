// Package snapshot implements the Phase 7 workspace snapshot / restore
// subsystem: zstd-compressed point-in-time copies of a sandbox's
// loopback `.img`, an hourly auto-snapshotter for sandboxes idle ≥
// 24 h (the "Snapshotted" row from CLAUDE.md's idle-lifecycle table),
// a retention pruner, and a restore path.
//
// roadmap/phase-7-monitoring-snapshots-and-ops.md §9–§10.
//
// Layout on disk:
//
//	/var/lib/sandboxed/_snapshots/<id>/<YYYY-MM-DD-HHMMSS>.img.zst
//	/var/lib/sandboxed/_snapshots/<id>/<YYYY-MM-DD-HHMMSS>.json   (sidecar)
//
// Every snapshot is written `.tmp` then atomically renamed; a partial
// `.tmp` left by a crash is swept by the reconciler on next boot.
package snapshot

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/idlock"
	"github.com/sandboxd/control-plane/internal/store"
)

// tsLayout is the timestamp format embedded in snapshot filenames.
// Sorts lexicographically in chronological order — the pruner relies
// on that.
const tsLayout = "2006-01-02-150405"

// Manager owns the snapshot subsystem's configuration + collaborators.
type Manager struct {
	WorkspacesRoot string        // /var/lib/sandboxed/workspaces
	SnapshotsRoot  string        // /var/lib/sandboxed/_snapshots
	RetentionDays  int           // SANDBOXD_SNAPSHOT_RETENTION_DAYS (7)
	IdleThreshold  time.Duration // auto-snapshot a stopped sandbox once idle this long (24h)

	Store *store.Store
	Locks *idlock.Registry
	Log   *slog.Logger
}

// Meta is the sidecar JSON written next to each snapshot.
type Meta struct {
	TS                  string `json:"ts"`
	ImageTag            string `json:"image_tag,omitempty"`
	SizeBytes           int64  `json:"size_bytes"`            // logical (apparent) size of the .img
	CompressedSizeBytes int64  `json:"compressed_size_bytes"` // size of the .img.zst
	HostCPUMillis       int64  `json:"host_cpu_ms"`           // zstd child user+sys CPU
	TakenAt             string `json:"taken_at"`
	Auto                bool   `json:"auto"` // true if the hourly snapshotter took it
}

// Entry is one item in a List() result.
type Entry struct {
	TS                  string `json:"ts"`
	SizeBytes           int64  `json:"size_bytes"`
	CompressedSizeBytes int64  `json:"compressed_size_bytes"`
	Auto                bool   `json:"auto"`
}

// Sentinel errors for callers (the API layer maps them to HTTP codes).
var (
	ErrNoImg    = fmt.Errorf("snapshot: no .img on disk for this id")
	ErrRunning  = fmt.Errorf("snapshot: sandbox row is running; DELETE it first")
	ErrNotFound = fmt.Errorf("snapshot: snapshot not found")
)

// imgPath / mntPath mirror loopback.Manager.Paths so snapshot doesn't
// have to depend on that package just for two string joins.
func (m *Manager) imgPath(id string) string {
	return filepath.Join(m.WorkspacesRoot, id+".img")
}
func (m *Manager) mntPath(id string) string {
	return filepath.Join(m.WorkspacesRoot, id+".mnt")
}
func (m *Manager) dir(id string) string {
	return filepath.Join(m.SnapshotsRoot, id)
}
func (m *Manager) zstPath(id, ts string) string {
	return filepath.Join(m.dir(id), ts+".img.zst")
}
func (m *Manager) metaPath(id, ts string) string {
	return filepath.Join(m.dir(id), ts+".json")
}

// imgExists reports whether the workspace `.img` is on disk.
func (m *Manager) imgExists(id string) bool {
	_, err := os.Stat(m.imgPath(id))
	return err == nil
}

// List returns the snapshots on disk for id, newest first. It does
// NOT consult the DB — roadmap §9: "Operates against
// _snapshots/<id>/ directly ... Does not require a sandbox row."
// Returns an empty slice if the directory is absent or empty.
func (m *Manager) List(id string) ([]Entry, error) {
	entries, err := os.ReadDir(m.dir(id))
	if err != nil {
		if os.IsNotExist(err) {
			return []Entry{}, nil
		}
		return nil, err
	}
	out := []Entry{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".img.zst") {
			continue
		}
		ts := strings.TrimSuffix(name, ".img.zst")
		ent := Entry{TS: ts}
		if fi, err := e.Info(); err == nil {
			ent.CompressedSizeBytes = fi.Size()
		}
		// Pull logical size + auto flag from the sidecar when present.
		if meta, err := m.readMeta(id, ts); err == nil {
			ent.SizeBytes = meta.SizeBytes
			ent.Auto = meta.Auto
		}
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out, nil
}

func (m *Manager) readMeta(id, ts string) (Meta, error) {
	var meta Meta
	b, err := os.ReadFile(m.metaPath(id, ts))
	if err != nil {
		return meta, err
	}
	err = json.Unmarshal(b, &meta)
	return meta, err
}

// listTimestamps returns just the snapshot timestamps for id, newest
// first. Used by the pruner.
func (m *Manager) listTimestamps(id string) ([]string, error) {
	ents, err := m.List(id)
	if err != nil {
		return nil, err
	}
	ts := make([]string, 0, len(ents))
	for _, e := range ents {
		ts = append(ts, e.TS)
	}
	return ts, nil
}
