package activity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// Tailer reads Traefik's JSON access log and, for each line whose
// RequestHost matches the preview-URL shape, bumps last_active_at
// on the matching sandbox row.
//
// Robustness rules per roadmap §3:
//   - Survive file rotation: detect via inode change + EOF; reopen
//     the file at offset 0.
//   - Survive truncation: detect via current-size < last-read-offset;
//     reopen at 0.
//   - Checkpoint the byte offset to /var/lib/sandboxed/state/
//     so a daemon restart doesn't replay the entire log. A stale
//     checkpoint is fine — we only ever move last_active_at forward.
//
// The handler is deliberately tolerant of malformed lines (logrotate
// can write partial trailing bytes during rotation). A line that
// doesn't parse as JSON is logged at debug-level and skipped.
type Tailer struct {
	LogPath        string         // /var/log/sandboxed/traefik-access.log
	CheckpointPath string         // /var/lib/sandboxed/state/traefik-tail.offset
	PreviewDomain  string         // example.com
	Store          *store.Store
	Log            *slog.Logger

	hostRE     *regexp.Regexp // ^s-([0-9A-Za-z]+)-([0-9]+)\.preview\.<domain>(:.*)?$
	mu         sync.Mutex
	lastBumped time.Time
}

// Run blocks until ctx is cancelled. It opens the access log,
// catches up to the checkpoint (if any), then tails new lines.
func (t *Tailer) Run(ctx context.Context) error {
	if err := t.compileHostRE(); err != nil {
		return err
	}
	for {
		if err := t.runOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			t.Log.Warn("tailer: run errored; retrying in 5s", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func (t *Tailer) compileHostRE() error {
	// Match `s-<id>-<port>.preview.<domain>` with an optional `:port`
	// suffix (some proxies include it in the Host header).
	pat := `^s-([0-9A-Za-z]+)-([0-9]+)\.preview\.` +
		regexp.QuoteMeta(t.PreviewDomain) + `(?::\d+)?$`
	re, err := regexp.Compile(pat)
	if err != nil {
		return fmt.Errorf("compile preview-host regex: %w", err)
	}
	t.hostRE = re
	return nil
}

// runOnce tries to open and tail the access log. Returns on
// context cancellation or unrecoverable IO error.
func (t *Tailer) runOnce(ctx context.Context) error {
	// Wait for the file to exist on first start — Traefik writes it
	// lazily on the first request.
	for {
		if _, err := os.Stat(t.LogPath); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	f, err := os.Open(t.LogPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", t.LogPath, err)
	}
	defer f.Close()

	info0, err := f.Stat()
	if err != nil {
		return err
	}
	inode0 := statInode(info0)

	// Seek to checkpoint if it exists and is sane.
	if off := t.readCheckpoint(); off > 0 {
		if off > info0.Size() {
			// truncation/rotation since the checkpoint — start over.
			off = 0
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			t.Log.Warn("tailer: seek to checkpoint failed; starting at offset 0", "err", err.Error())
			_, _ = f.Seek(0, io.SeekStart)
		}
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Process all available complete lines.
		for {
			line, err := reader.ReadString('\n')
			if errors.Is(err, io.EOF) {
				// Save offset for the partial-line case (we re-read
				// from the start of the partial line on next pass).
				break
			}
			if err != nil {
				return fmt.Errorf("read: %w", err)
			}
			t.handleLine(ctx, strings.TrimRight(line, "\n"))
		}
		// Persist offset for restart safety.
		if cur, err := f.Seek(0, io.SeekCurrent); err == nil {
			t.writeCheckpoint(cur)
		}

		// Wait for more data, but also re-check for rotation/truncation.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		// Detect rotation (inode change) or truncation (size shrink).
		info1, err := os.Stat(t.LogPath)
		if err != nil {
			// Log gone; logrotate may be mid-move. Loop back and
			// re-open from scratch.
			return nil
		}
		if statInode(info1) != inode0 || info1.Size() < info0.Size() {
			t.Log.Info("tailer: rotation/truncation detected; re-opening",
				"old_inode", inode0,
				"new_inode", statInode(info1),
				"old_size", info0.Size(),
				"new_size", info1.Size())
			// Reset checkpoint and re-open from offset 0.
			t.writeCheckpoint(0)
			return nil
		}
		info0 = info1
	}
}

// accessLogLine is the slim slice of Traefik's JSON access log we read.
// Traefik emits many more fields; encoding/json ignores extras.
type accessLogLine struct {
	RequestHost   string `json:"RequestHost"`
	RequestMethod string `json:"RequestMethod"`
	OriginStatus  int    `json:"OriginStatus"`
	StartUTC      string `json:"StartUTC"`
	RouterName    string `json:"RouterName"`
}

func (t *Tailer) handleLine(ctx context.Context, line string) {
	if line == "" {
		return
	}
	var l accessLogLine
	if err := json.Unmarshal([]byte(line), &l); err != nil {
		// Partial / non-JSON line. logrotate moments emit these.
		return
	}
	if l.RequestHost == "" {
		return
	}
	m := t.hostRE.FindStringSubmatch(l.RequestHost)
	if m == nil {
		return
	}
	id := m[1]
	port := m[2]
	// We don't actually need port here, but parse-validate it so a
	// malformed RequestHost doesn't slip through.
	if _, err := strconv.Atoi(port); err != nil {
		return
	}

	now := time.Now().UTC()
	if err := t.Store.BumpLastActive(ctx, id, now); err != nil {
		t.Log.Warn("tailer: BumpLastActive failed",
			"sandbox_id", id, "err", err.Error())
		return
	}

	// Record the access-log lag metric (now − StartUTC if parseable).
	if l.StartUTC != "" {
		if start, err := time.Parse(time.RFC3339Nano, l.StartUTC); err == nil {
			metrics.AccessLogLagSeconds.Set(time.Since(start).Seconds())
		}
	}

	t.mu.Lock()
	t.lastBumped = now
	t.mu.Unlock()
}

func (t *Tailer) readCheckpoint() int64 {
	b, err := os.ReadFile(t.CheckpointPath)
	if err != nil {
		return 0
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || off < 0 {
		return 0
	}
	return off
}

func (t *Tailer) writeCheckpoint(off int64) {
	dir := filepath.Dir(t.CheckpointPath)
	_ = os.MkdirAll(dir, 0o750)
	tmp := t.CheckpointPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o640); err != nil {
		return
	}
	_ = os.Rename(tmp, t.CheckpointPath)
}
