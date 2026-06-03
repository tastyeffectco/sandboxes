package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// A dev-server run shorter than this counts as a fast failure.
	fastFailWindow = 10 * time.Second
	// After this many consecutive fast failures the supervisor stops
	// restarting (a hopelessly broken app — reported down, not
	// crash-looped).
	maxFastFails = 5
	maxBackoff   = 30 * time.Second
)

// devServer supervises the Vite dev server: one dev-server child kept
// running, restarted with exponential backoff on unexpected exit, and
// abandoned after repeated fast failures.
type devServer struct {
	appDir  string
	devCmd  string // shell command, run via `bash -lc`
	logPath string
	log     *slog.Logger

	mu       sync.Mutex
	proc     *os.Process
	running  bool
	restarts int
}

func newDevServer(appDir, devCmd, logPath string, log *slog.Logger) *devServer {
	return &devServer{appDir: appDir, devCmd: devCmd, logPath: logPath, log: log}
}

// supervise is the dev server's whole lifecycle; it runs until ctx is
// cancelled (runtimed shutdown).
func (d *devServer) supervise(ctx context.Context) {
	fastFails := 0
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		d.runOnce()
		if ctx.Err() != nil {
			return // intentional shutdown — do not restart
		}
		d.mu.Lock()
		d.restarts++
		restarts := d.restarts
		d.mu.Unlock()
		if time.Since(start) < fastFailWindow {
			fastFails++
		} else {
			fastFails = 0
		}
		if fastFails >= maxFastFails {
			d.log.Error("dev server failing repeatedly — giving up until next start",
				"restarts", restarts)
			return
		}
		delay := backoff(fastFails)
		d.log.Warn("dev server exited; restarting after backoff",
			"delay", delay.String(), "restarts", restarts)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
}

// runOnce starts the dev server, records it as live, and blocks until
// it exits.
func (d *devServer) runOnce() {
	// `bash -lc` so the login PATH (pnpm, node) is in scope.
	cmd := exec.Command("bash", "-lc", d.devCmd)
	cmd.Dir = d.appDir
	// Own process group so the whole `bash → pnpm → node → vite` tree
	// can be signalled as a unit on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if f, err := os.Create(d.logPath); err == nil {
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close()
	} else {
		d.log.Warn("dev-server log file", "path", d.logPath, "err", err.Error())
	}
	if err := cmd.Start(); err != nil {
		d.log.Error("dev server start failed", "err", err.Error())
		return
	}
	d.mu.Lock()
	d.proc = cmd.Process
	d.running = true
	d.mu.Unlock()
	d.log.Info("dev server started", "pid", cmd.Process.Pid)

	_ = cmd.Wait()

	d.mu.Lock()
	d.proc = nil
	d.running = false
	d.mu.Unlock()
	d.log.Info("dev server exited")
}

// stop terminates the dev server's process group: SIGTERM, then
// SIGKILL if it has not exited within the grace window.
func (d *devServer) stop() {
	d.mu.Lock()
	p := d.proc
	d.mu.Unlock()
	if p == nil {
		return
	}
	pgid := p.Pid // == process group id (Setpgid made it the leader)
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	for i := 0; i < 50; i++ { // up to ~5s
		time.Sleep(100 * time.Millisecond)
		d.mu.Lock()
		running := d.running
		d.mu.Unlock()
		if !running {
			return
		}
	}
	d.log.Warn("dev server did not exit on SIGTERM; sending SIGKILL")
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// snapshot returns the current dev-server state for GET /status.
func (d *devServer) snapshot() (pid, restarts int, running bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.proc != nil {
		pid = d.proc.Pid
	}
	return pid, d.restarts, d.running
}

// backoff is exponential in the consecutive-fast-failure count,
// capped at maxBackoff.
func backoff(fastFails int) time.Duration {
	if fastFails < 1 {
		fastFails = 1
	}
	d := time.Second << (fastFails - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
