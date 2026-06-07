package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// Take produces one snapshot of sandbox id's loopback `.img`.
//
// Preconditions (roadmap §9 manual-snapshot table):
//   - the on-disk `.img` must exist            → else ErrNoImg
//   - if a DB row exists, it must NOT be running → else ErrRunning
//   - no row at all is fine (orphan workspace)  → proceeds
//
// Take acquires the per-id lock for the whole procedure so it cannot
// race a wake (which would start the container and write to the very
// loopback we are compressing). `auto` marks an auto-snapshotter run
// in the sidecar.
func (m *Manager) Take(ctx context.Context, id string, auto bool) (Meta, error) {
	m.Locks.Lock(id)
	defer m.Locks.Unlock(id)
	return m.takeLocked(ctx, id, auto)
}

// takeLocked is the body; the caller already holds the per-id lock.
func (m *Manager) takeLocked(ctx context.Context, id string, auto bool) (Meta, error) {
	log := m.Log.With("sandbox_id", id, "auto", auto)

	// Precondition: .img must exist.
	if !m.imgExists(id) {
		return Meta{}, ErrNoImg
	}
	// Precondition: re-check the row state under the lock. A row that
	// is `running` is rejected; `stopped`/`error`/absent all proceed.
	var imageTag string
	if sb, err := m.Store.Get(ctx, id); err == nil {
		if sb.Status == "running" {
			return Meta{}, ErrRunning
		}
		imageTag = sb.Image
	} else if !errors.Is(err, store.ErrNotFound) {
		return Meta{}, fmt.Errorf("snapshot: store.Get: %w", err)
	}

	// Flush host pagecache so the loopback file on disk reflects every
	// write. The container is stopped (precondition), so nothing is
	// actively writing — sync + no-writers gives a crash-consistent
	// image. roadmap §"Risks": nothing writes a stopped sandbox's
	// loopback except sandboxd, and sandboxd doesn't snapshot while
	// it writes (this lock guarantees it).
	_ = exec.CommandContext(ctx, "sync").Run()

	ts := time.Now().UTC().Format(tsLayout)
	if err := os.MkdirAll(m.dir(id), 0o750); err != nil {
		return Meta{}, fmt.Errorf("snapshot: mkdir %s: %w", m.dir(id), err)
	}
	img := m.imgPath(id)
	final := m.zstPath(id, ts)
	tmp := final + ".tmp"

	imgInfo, err := os.Stat(img)
	if err != nil {
		return Meta{}, fmt.Errorf("snapshot: stat img: %w", err)
	}

	// Stream-compress at the zstd default level (3) — fast, and an
	// 8 GB sparse loopback that's mostly empty compresses well even at
	// level 3. -T2 caps zstd to 2 threads so a snapshot of one sandbox
	// doesn't monopolise CPU another sandbox's wake needs. -f forces
	// past a `.tmp` lingering from a crash mid-snapshot.
	start := time.Now()
	cmd := exec.CommandContext(ctx, "zstd", "-q", "-f", "-T2", "-o", tmp, img)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		metrics.SnapshotsTaken.WithLabelValues("error").Inc()
		return Meta{}, fmt.Errorf("snapshot: zstd: %w (%s)", err, string(out))
	}
	cpuMS := int64(0)
	if cmd.ProcessState != nil {
		cpuMS = (cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()).Milliseconds()
	}

	// fsync the .tmp, then atomically rename, then fsync the directory
	// so the rename is durable.
	if err := fsyncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return Meta{}, fmt.Errorf("snapshot: fsync tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return Meta{}, fmt.Errorf("snapshot: rename: %w", err)
	}
	_ = fsyncDir(m.dir(id))

	zInfo, err := os.Stat(final)
	if err != nil {
		return Meta{}, fmt.Errorf("snapshot: stat zst: %w", err)
	}

	meta := Meta{
		TS:                  ts,
		ImageTag:            imageTag,
		SizeBytes:           imgInfo.Size(),
		CompressedSizeBytes: zInfo.Size(),
		HostCPUMillis:       cpuMS,
		TakenAt:             time.Now().UTC().Format(time.RFC3339),
		Auto:                auto,
	}
	if mb, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(m.metaPath(id, ts), mb, 0o640)
	}

	dur := time.Since(start)
	metrics.SnapshotsTaken.WithLabelValues("ok").Inc()
	metrics.SnapshotLastDurationSeconds.Set(dur.Seconds())
	metrics.SnapshotLastSizeBytes.Set(float64(zInfo.Size()))
	log.Info("snapshot: taken",
		"ts", ts,
		"img_bytes", imgInfo.Size(),
		"zst_bytes", zInfo.Size(),
		"duration_ms", dur.Milliseconds(),
	)

	// Prune under the same lock so two snapshots of the same id can't
	// interleave their retention math.
	if err := m.pruneLocked(id); err != nil {
		log.Warn("snapshot: prune after take failed", "err", err.Error())
	}
	return meta, nil
}

// fsyncFile opens path, fsyncs, closes.
func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// fsyncDir fsyncs a directory so a rename within it is durable.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
