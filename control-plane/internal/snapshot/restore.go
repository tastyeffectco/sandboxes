package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// RestoreResult is returned to the API caller on success.
type RestoreResult struct {
	SizeBytes  int64  `json:"size_bytes"`
	RestoredAt string `json:"restored_at"`
}

// Restore puts a snapshot back as the live `.img` for sandbox id.
//
// roadmap §10. Preconditions:
//   - the snapshot `<ts>.img.zst` must exist → else ErrNotFound
//   - if a DB row exists it must be stopped/error → running ⇒ ErrRunning
//   - no row at all is fine — restore works on the on-disk `.img`.
//
// The current `.img` is moved aside to `.img.bak-<now>` and only
// deleted once `fsck.ext4 -n` confirms the restored image is clean.
// An interrupted restore is recoverable from the `.bak`.
func (m *Manager) Restore(ctx context.Context, id, ts string) (RestoreResult, error) {
	m.Locks.Lock(id)
	defer m.Locks.Unlock(id)
	log := m.Log.With("sandbox_id", id, "snapshot", ts)

	zst := m.zstPath(id, ts)
	if _, err := os.Stat(zst); err != nil {
		return RestoreResult{}, ErrNotFound
	}
	// Row state check: running is rejected.
	if sb, err := m.Store.Get(ctx, id); err == nil {
		if sb.Status == "running" {
			return RestoreResult{}, ErrRunning
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return RestoreResult{}, fmt.Errorf("restore: store.Get: %w", err)
	}

	img := m.imgPath(id)
	mnt := m.mntPath(id)

	// 1. Unmount the current loopback if mounted.
	if mounted, _ := isMounted(mnt); mounted {
		if out, err := exec.CommandContext(ctx, "umount", mnt).CombinedOutput(); err != nil {
			return RestoreResult{}, fmt.Errorf("restore: umount %s: %w (%s)", mnt, err, strings.TrimSpace(string(out)))
		}
	}

	// 2. Move the current .img aside (kept until fsck validates).
	bak := ""
	if _, err := os.Stat(img); err == nil {
		bak = fmt.Sprintf("%s.bak-%d", img, time.Now().Unix())
		if err := os.Rename(img, bak); err != nil {
			return RestoreResult{}, fmt.Errorf("restore: move current img aside: %w", err)
		}
	}
	// abort restores the .bak and returns the wrapped error.
	abort := func(stage string, cause error) (RestoreResult, error) {
		_ = os.Remove(img)
		if bak != "" {
			_ = os.Rename(bak, img)
		}
		metrics.SnapshotRestores.WithLabelValues("error").Inc()
		log.Error("restore: aborted; .img rolled back", "stage", stage, "err", cause.Error())
		return RestoreResult{}, fmt.Errorf("restore: %s: %w", stage, cause)
	}

	// 3. Stream-decompress. --sparse rebuilds hole semantics on the
	// regular-file output (zstd disables sparse for non-stdout output
	// unless asked).
	if out, err := exec.CommandContext(ctx, "zstd", "-q", "-f", "-d", "--sparse",
		"-o", img, zst).CombinedOutput(); err != nil {
		return abort("zstd decompress", fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out))))
	}

	// 4. Dry-run fsck. We do NOT auto-repair (-y) — a silent repair is
	// worse than a loud failure (roadmap §"Risks").
	fsckOut, fsckErr := exec.CommandContext(ctx, "fsck.ext4", "-n", img).CombinedOutput()
	if fsckErr != nil {
		// fsck.ext4 exit codes: 0 clean, 1 errors corrected (n/a in -n),
		// 4 errors left uncorrected, 8 operational error. Anything
		// non-zero on a dry run means the restored image is suspect.
		return abort("fsck dry-run", fmt.Errorf("restored image failed fsck: %s", strings.TrimSpace(string(fsckOut))))
	}

	// 5. Mount the restored image.
	if out, err := exec.CommandContext(ctx, "mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		return abort("mount", fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out))))
	}

	// 6. Sanity: /opt/sandbox-skel must NOT be at the mount root — that
	// directory belongs in the base image, never in a user workspace.
	// Its presence would mean we restored the wrong thing.
	if _, err := os.Stat(mnt + "/opt/sandbox-skel"); err == nil {
		_ = exec.CommandContext(ctx, "umount", mnt).Run()
		return abort("sanity check", fmt.Errorf("/opt/sandbox-skel present at mount root — restored image looks wrong"))
	}

	// 7. Restore validated — drop the .bak.
	if bak != "" {
		_ = os.Remove(bak)
	}

	info, _ := os.Stat(img)
	var size int64
	if info != nil {
		size = info.Size()
	}
	metrics.SnapshotRestores.WithLabelValues("ok").Inc()
	log.Info("restore: complete", "size_bytes", size)
	return RestoreResult{
		SizeBytes:  size,
		RestoredAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// isMounted checks /proc/mounts for an exact mountpoint match.
func isMounted(mnt string) (bool, error) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == mnt {
			return true, nil
		}
	}
	return false, nil
}
