// Command runtimed is the in-sandbox supervisor.
//
// Scope of this slice (slice 1): run as the container's main process,
// start and supervise the Vite dev server, and expose GET /status
// over a Unix domain socket on the workspace loopback. The
// coding-task subsystem (POST /tasks, events, cancel) is a later
// slice — see cmd/runtimed/README.md and ops/design/v1-external-api.md.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sandboxd/control-plane/internal/runtime"
)

const version = "0.1.0"

// app holds runtimed's live state: the dev-server supervisor, the most
// recent preview health probe, and the one active coding task.
type app struct {
	dev         *devServer
	previewPort int
	appDir      string
	runtimeDir  string
	log         *slog.Logger
	bootedAt    time.Time

	mu           sync.Mutex
	lastCode     int
	lastAssetErr string // entry-asset compile error, "" when assets transform
	lastChecked  time.Time

	taskMu sync.Mutex // guards task; serializes start (one task at a time)
	task   *task      // the current / last task, or nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "runtimed")

	appDir := envOr("RUNTIMED_APP_DIR", "/home/sandbox/workspace/app")
	runtimeDir := envOr("RUNTIMED_DIR", "/home/sandbox/.runtimed")
	socketPath := envOr("RUNTIMED_SOCKET", filepath.Join(runtimeDir, "sock"))
	devCmd := envOr("RUNTIMED_DEV_CMD", "pnpm dev")
	previewPort := envOrInt("RUNTIMED_PREVIEW_PORT", 3000)
	probeInterval := time.Duration(envOrInt("RUNTIMED_PROBE_INTERVAL_SECONDS", 3)) * time.Second

	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		log.Error("mkdir runtime dir", "dir", runtimeDir, "err", err.Error())
		os.Exit(1)
	}
	// Ensure the app working directory exists. A fresh workspace ships
	// with only ~/workspace, so ~/workspace/app may not exist yet. Both
	// the managed dev server and the coding-agent runner chdir into
	// appDir — a missing dir makes fork/exec fail with a misleading
	// ENOENT against the binary. Creating it here self-heals every
	// sandbox on start (including ones provisioned before this fix).
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		log.Error("mkdir app dir", "dir", appDir, "err", err.Error())
		os.Exit(1)
	}

	a := &app{
		dev:         newDevServer(appDir, devCmd, filepath.Join(runtimeDir, "dev-server.log"), log),
		previewPort: previewPort,
		appDir:      appDir,
		runtimeDir:  runtimeDir,
		log:         log,
		bootedAt:    time.Now(),
	}

	// Finalize any task interrupted by a previous stop/crash before
	// accepting new work — an interrupted task is failed, never resumed.
	recoverInterruptedTasks(filepath.Join(runtimeDir, "tasks"), log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go a.dev.supervise(ctx)
	go a.probeLoop(ctx, probeInterval)

	log.Info("runtimed started", "version", version, "app_dir", appDir, "socket", socketPath)
	if err := serve(ctx, socketPath, a); err != nil {
		log.Error("control server", "err", err.Error())
	}

	// ctx is done — shut the dev server down cleanly before exiting.
	log.Info("runtimed shutting down — stopping dev server")
	a.dev.stop()
	log.Info("runtimed stopped")
}

// probeLoop polls the dev server's HTTP port so /status reports a real
// readiness signal rather than just process liveness.
func (a *app) probeLoop(ctx context.Context, interval time.Duration) {
	a.probe()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.probe()
		}
	}
}

func (a *app) probe() {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	code, _ := a.devGet(ctx, "/")
	// The HTML shell can serve 200 while the dev server fails to
	// transform the real entry modules (a blank page). Only probe the
	// entry assets once the shell is up, so `error` means "renders the
	// shell but the app is broken", not "still starting".
	assetErr := ""
	if code == 200 {
		assetErr = a.probeEntryAssets(ctx)
	}
	a.mu.Lock()
	a.lastCode = code
	a.lastAssetErr = assetErr
	a.lastChecked = time.Now()
	a.mu.Unlock()
}

// status derives the runtime.Status snapshot from the dev-server
// process state and the latest health probe.
func (a *app) status() runtime.Status {
	pid, restarts, running := a.dev.snapshot()
	a.mu.Lock()
	code, assetErr, checked := a.lastCode, a.lastAssetErr, a.lastChecked
	a.mu.Unlock()

	ps := runtime.PreviewState{Restarts: restarts, LastHTTPStatus: code}
	switch {
	case !running:
		ps.Status = runtime.PreviewDown
	case code == 200 && assetErr == "":
		ps.Status = runtime.PreviewReady
		ps.Pid = pid
	case code == 200 && assetErr != "":
		// shell serves but an entry module fails to compile — a blank page.
		ps.Status = runtime.PreviewError
		ps.BuildErrorMessage = assetErr
		ps.Pid = pid
	default:
		ps.Status = runtime.PreviewStarting
		ps.Pid = pid
	}
	if !checked.IsZero() {
		c := checked
		ps.LastCheckedAt = &c
	}
	return runtime.Status{
		Runtimed: runtime.RuntimedInfo{
			Version:  version,
			BootedAt: a.bootedAt,
			UptimeS:  int64(time.Since(a.bootedAt).Seconds()),
		},
		Preview:    ps,
		ActiveTask: a.activeTaskRef(),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
