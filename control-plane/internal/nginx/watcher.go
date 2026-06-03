// Package nginx provides a polling filesystem watcher that
// safe-reloads nginx (running as a Docker container) on changes to
// operator-managed config under /etc/sandboxed/nginx/.
//
// Design notes
//
//   - Polling (not inotify) by deliberate choice — mirrors the
//     existing `internal/egress.RefreshWatcher` pattern, adds no
//     dependency, is robust to missed events, and operators editing
//     config files via SSH are not latency-sensitive to milliseconds.
//   - Safe-reload semantics: every detected change triggers
//     `nginx -t`; only on success do we run `nginx -s reload`. A bad
//     edit therefore never reaches running workers — they keep
//     serving the prior config and the operator gets a log line.
//     (This precisely prevents the "malformed opencode-go.conf"
//     incident — workers stayed up; would have refused to load on
//     reload; the new bad config never reached them.)
//   - No state on disk: the watcher computes a content fingerprint
//     each tick and compares to the previous one. A restart of
//     sandboxd will trigger one extra (idempotent) reload on first
//     tick, which is fine.
//   - Idempotency: nginx reload is cheap and is itself a no-op if
//     the config is identical to what workers already have. We do
//     not optimise around that.
package nginx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultInterval is the polling cadence.
const DefaultInterval = 5 * time.Second

// Execer is the subset of docker.Client this package needs. The
// indirection makes the watcher unit-testable without a real Docker
// daemon.
type Execer interface {
	Exec(ctx context.Context, container string, cmd []string) (ExecResult, error)
}

// ExecResult mirrors docker.ExecResult to avoid an import cycle in
// tests; an adapter in main.go bridges *docker.Client to Execer.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Watcher watches one or more filesystem paths (files OR directories)
// and reloads nginx in a target container when contents change.
type Watcher struct {
	// Paths is the set of files and/or directories to watch. Symlinks
	// are followed. Missing paths are tolerated (logged once per tick).
	Paths []string

	// Container is the nginx container name (e.g. "sandbox-registry-proxy").
	Container string

	// Exec is the docker.Client adapter; required.
	Exec Execer

	// Log is the structured logger. Required.
	Log *slog.Logger

	// Interval is the poll cadence. Zero → DefaultInterval.
	Interval time.Duration

	// Hooks for tests + metrics. Both optional.
	OnReloadOK   func()
	OnReloadFail func(reason string)

	last string // last computed fingerprint
}

// Run blocks until ctx is cancelled. It returns ctx.Err() at the
// end.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Interval == 0 {
		w.Interval = DefaultInterval
	}
	// Compute initial fingerprint without reloading — workers already
	// have the matching config, by assumption (they just started).
	w.last = w.fingerprint()
	w.Log.Info("nginx watcher started",
		"paths", w.Paths, "container", w.Container, "interval", w.Interval)

	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			cur := w.fingerprint()
			if cur == w.last {
				continue
			}
			w.Log.Info("nginx watcher: change detected, validating", "old_fp", w.last[:12], "new_fp", cur[:12])
			if w.reload(ctx) {
				w.last = cur
			}
			// On reload failure, we deliberately do NOT update last —
			// so the next tick will try again. Operators usually fix
			// forward; we want the reload to happen as soon as they do.
		}
	}
}

// fingerprint returns a deterministic content+metadata digest across
// all watched paths. Directories contribute their entries (recursive
// one level deep — typical config layouts) with size+mtime; files
// contribute size+mtime.
func (w *Watcher) fingerprint() string {
	h := sha256.New()
	for _, p := range w.Paths {
		w.addPath(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (w *Watcher) addPath(h hash.Hash, p string) {
	fi, err := os.Stat(p)
	if err != nil {
		// Missing paths are not an error condition — operators may not
		// have created credentials.d/ yet. Contribute a sentinel so
		// later existence flips the fingerprint.
		fmt.Fprintf(h, "MISSING|%s\n", p)
		return
	}
	if !fi.IsDir() {
		fmt.Fprintf(h, "FILE|%s|%d|%d\n", p, fi.Size(), fi.ModTime().UnixNano())
		return
	}
	// Directory — sorted listing for determinism.
	entries, err := os.ReadDir(p)
	if err != nil {
		fmt.Fprintf(h, "DIR_ERR|%s|%s\n", p, err.Error())
		return
	}
	names := make([]string, 0, len(entries))
	byName := map[string]fs.DirEntry{}
	for _, e := range entries {
		names = append(names, e.Name())
		byName[e.Name()] = e
	}
	sort.Strings(names)
	for _, name := range names {
		e := byName[name]
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(h, "DIR_ITEM_ERR|%s/%s|%s\n", p, name, err.Error())
			continue
		}
		fmt.Fprintf(h, "DIR_ITEM|%s/%s|%d|%d\n", p, name, info.Size(), info.ModTime().UnixNano())
	}
}

// reload runs `nginx -t` and, on success, `nginx -s reload`. Returns
// true when the reload was successfully issued (worker rollover is
// nginx's responsibility).
func (w *Watcher) reload(ctx context.Context) bool {
	tCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := w.Exec.Exec(tCtx, w.Container, []string{"nginx", "-t"})
	if err != nil {
		w.Log.Error("nginx -t failed to execute; not reloading", "err", err.Error())
		w.fail("exec_error")
		return false
	}
	if res.ExitCode != 0 {
		w.Log.Error("nginx -t reported invalid config; not reloading",
			"exit", res.ExitCode, "stderr_first_line", firstLine(res.Stderr))
		w.fail("validate_failed")
		return false
	}

	rCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	res2, err := w.Exec.Exec(rCtx, w.Container, []string{"nginx", "-s", "reload"})
	if err != nil {
		w.Log.Error("nginx -s reload failed to execute", "err", err.Error())
		w.fail("reload_exec_error")
		return false
	}
	if res2.ExitCode != 0 {
		w.Log.Error("nginx -s reload non-zero exit",
			"exit", res2.ExitCode, "stderr_first_line", firstLine(res2.Stderr))
		w.fail("reload_failed")
		return false
	}
	w.Log.Info("nginx reloaded (config change detected)")
	if w.OnReloadOK != nil {
		w.OnReloadOK()
	}
	return true
}

func (w *Watcher) fail(reason string) {
	if w.OnReloadFail != nil {
		w.OnReloadFail(reason)
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// Validate sanity-checks the configuration; returns nil if Run is safe
// to start.
func (w *Watcher) Validate() error {
	if len(w.Paths) == 0 {
		return errors.New("nginx watcher: at least one path is required")
	}
	if w.Container == "" {
		return errors.New("nginx watcher: container is required")
	}
	if w.Exec == nil {
		return errors.New("nginx watcher: Exec is required")
	}
	if w.Log == nil {
		return errors.New("nginx watcher: Log is required")
	}
	// Resolve to absolute paths to make logs unambiguous.
	for i, p := range w.Paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("nginx watcher: bad path %q: %w", p, err)
		}
		w.Paths[i] = abs
	}
	return nil
}
