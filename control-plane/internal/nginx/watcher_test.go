package nginx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExecer captures invocations and returns scripted results.
type fakeExecer struct {
	mu    sync.Mutex
	calls [][]string
	// scripted result by joined cmd
	results map[string]ExecResult
	errs    map[string]error
}

func newFakeExecer() *fakeExecer {
	return &fakeExecer{results: map[string]ExecResult{}, errs: map[string]error{}}
}

func (f *fakeExecer) Exec(_ context.Context, _ string, cmd []string) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	key := joinArgs(cmd)
	if err, ok := f.errs[key]; ok {
		return ExecResult{}, err
	}
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	return ExecResult{ExitCode: 0}, nil // default: success
}

func (f *fakeExecer) calledN() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func joinArgs(cmd []string) string {
	out := ""
	for i, a := range cmd {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- fingerprint ----------------------------------------------------

func TestFingerprint_DetectsContentChange(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.conf"), "v1")

	w := &Watcher{Paths: []string{dir}, Container: "c", Exec: newFakeExecer(), Log: discardLogger()}
	a := w.fingerprint()
	if a == "" {
		t.Fatal("empty fingerprint")
	}

	// touching mtime alone is enough to flip the fingerprint.
	time.Sleep(10 * time.Millisecond)
	mustWrite(t, filepath.Join(dir, "a.conf"), "v2")
	b := w.fingerprint()
	if a == b {
		t.Fatal("fingerprint did not change after rewrite")
	}
}

func TestFingerprint_NewFileFlips(t *testing.T) {
	dir := t.TempDir()
	w := &Watcher{Paths: []string{dir}, Container: "c", Exec: newFakeExecer(), Log: discardLogger()}
	a := w.fingerprint()
	mustWrite(t, filepath.Join(dir, "new.conf"), "x")
	if a == w.fingerprint() {
		t.Fatal("fingerprint did not change when a new file appeared")
	}
}

func TestFingerprint_MissingPathTolerated(t *testing.T) {
	w := &Watcher{
		Paths:     []string{"/no/such/dir/here"},
		Container: "c", Exec: newFakeExecer(), Log: discardLogger(),
	}
	fp := w.fingerprint()
	if fp == "" {
		t.Fatal("missing path should still produce a fingerprint")
	}
}

// --- reload semantics -----------------------------------------------

func TestReload_OK(t *testing.T) {
	fe := newFakeExecer()
	w := &Watcher{Paths: []string{t.TempDir()}, Container: "x", Exec: fe, Log: discardLogger()}
	if !w.reload(context.Background()) {
		t.Fatal("expected reload success")
	}
	if got := fe.calledN(); got != 2 {
		t.Fatalf("expected 2 exec calls (test + reload), got %d", got)
	}
	if fe.calls[0][1] != "-t" {
		t.Fatalf("first call should be `nginx -t`, got %v", fe.calls[0])
	}
	if fe.calls[1][1] != "-s" || fe.calls[1][2] != "reload" {
		t.Fatalf("second call should be `nginx -s reload`, got %v", fe.calls[1])
	}
}

func TestReload_RefusesWhenValidateFails(t *testing.T) {
	fe := newFakeExecer()
	fe.results["nginx -t"] = ExecResult{ExitCode: 1, Stderr: "nginx: emerg: …"}
	w := &Watcher{Paths: []string{t.TempDir()}, Container: "x", Exec: fe, Log: discardLogger()}
	if w.reload(context.Background()) {
		t.Fatal("expected reload to be refused")
	}
	if got := fe.calledN(); got != 1 {
		t.Fatalf("expected only `nginx -t` to be called, got %d calls: %v", got, fe.calls)
	}
}

func TestReload_HandlesExecError(t *testing.T) {
	fe := newFakeExecer()
	fe.errs["nginx -t"] = errors.New("docker daemon down")
	failures := atomic.Int64{}
	w := &Watcher{
		Paths: []string{t.TempDir()}, Container: "x", Exec: fe, Log: discardLogger(),
		OnReloadFail: func(reason string) {
			if reason != "exec_error" {
				t.Errorf("unexpected reason: %q", reason)
			}
			failures.Add(1)
		},
	}
	if w.reload(context.Background()) {
		t.Fatal("expected false on exec error")
	}
	if failures.Load() != 1 {
		t.Fatalf("expected one failure hook call, got %d", failures.Load())
	}
}

func TestReload_HandlesReloadExitNonZero(t *testing.T) {
	fe := newFakeExecer()
	fe.results["nginx -t"] = ExecResult{ExitCode: 0}
	fe.results["nginx -s reload"] = ExecResult{ExitCode: 1, Stderr: "reload broke"}
	w := &Watcher{Paths: []string{t.TempDir()}, Container: "x", Exec: fe, Log: discardLogger()}
	if w.reload(context.Background()) {
		t.Fatal("expected false when reload itself exited non-zero")
	}
	if got := fe.calledN(); got != 2 {
		t.Fatalf("expected 2 exec calls, got %d", got)
	}
}

// --- end-to-end watcher behaviour (no real docker) ------------------

func TestRun_ReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.conf"), "initial")

	fe := newFakeExecer()
	okCh := make(chan struct{}, 4)
	w := &Watcher{
		Paths: []string{dir}, Container: "c", Exec: fe, Log: discardLogger(),
		Interval:   30 * time.Millisecond,
		OnReloadOK: func() { okCh <- struct{}{} },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	// first tick should NOT reload (initial fingerprint matches).
	time.Sleep(60 * time.Millisecond)
	if got := fe.calledN(); got != 0 {
		t.Fatalf("expected 0 exec calls before any change, got %d", got)
	}

	// touch the file → fingerprint flips → reload.
	time.Sleep(15 * time.Millisecond)
	mustWrite(t, filepath.Join(dir, "a.conf"), "changed")

	select {
	case <-okCh:
		// good
	case <-time.After(2 * time.Second):
		t.Fatalf("OnReloadOK never fired; calls=%v", fe.calls)
	}
}

func TestRun_DoesNotUpdateLastOnFailure(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.conf"), "v1")

	fe := newFakeExecer()
	fe.results["nginx -t"] = ExecResult{ExitCode: 1} // always fails

	failCh := make(chan struct{}, 8)
	w := &Watcher{
		Paths: []string{dir}, Container: "c", Exec: fe, Log: discardLogger(),
		Interval:     20 * time.Millisecond,
		OnReloadFail: func(string) { failCh <- struct{}{} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// change once
	time.Sleep(40 * time.Millisecond)
	mustWrite(t, filepath.Join(dir, "a.conf"), "v2")

	// wait for at least 2 failures — proves last is NOT being advanced
	// so the watcher keeps retrying until the operator fixes the file.
	deadline := time.Now().Add(2 * time.Second)
	count := 0
	for time.Now().Before(deadline) && count < 2 {
		select {
		case <-failCh:
			count++
		case <-time.After(100 * time.Millisecond):
		}
	}
	if count < 2 {
		t.Fatalf("expected ≥2 failure callbacks, got %d", count)
	}
}

func TestValidate(t *testing.T) {
	bad := []*Watcher{
		{},
		{Paths: []string{"/x"}},
		{Paths: []string{"/x"}, Container: "c"},
		{Paths: []string{"/x"}, Container: "c", Exec: newFakeExecer()},
	}
	for i, w := range bad {
		if err := w.Validate(); err == nil {
			t.Errorf("[bad #%d] expected validate error, got nil", i)
		}
	}
	good := &Watcher{
		Paths: []string{"/x"}, Container: "c",
		Exec: newFakeExecer(), Log: discardLogger(),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected validate ok, got %v", err)
	}
}

// --- helpers --------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
