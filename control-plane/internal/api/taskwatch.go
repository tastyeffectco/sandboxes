package api

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sandboxd/control-plane/internal/runtime"
)

const (
	taskWatchTimeout  = 15 * time.Minute // outlives the runtimed task timeout
	taskWatchAttempts = 3
)

// failedResult builds a clean terminal result for a task that could
// not complete normally. failure_reason is always an existing model
// value (sandbox_unavailable / internal) — no new terminal semantics
// are introduced.
func failedResult(taskID, reason, msg string) *runtime.TaskResult {
	return &runtime.TaskResult{
		ID:            taskID,
		Status:        runtime.TaskFailed,
		FailureReason: reason,
		ErrorMessage:  msg,
		FilesChanged:  []string{},
	}
}

// watchTask runs in the background for the lifetime of a coding task:
// it streams runtimed's event log and persists the canonical result
// to SQLite when the terminal `done` event arrives — independent of
// whether any client ever connects to the public events stream. This
// is what makes a task's result durable past the sandbox's lifetime.
func (s *Server) watchTask(sandboxID, taskID string) {
	log := s.Log.With("component", "taskwatch", "task", taskID)
	ctx, cancel := context.WithTimeout(context.Background(), taskWatchTimeout)
	defer cancel()

	rc := s.runtimeClientFor(sandboxID)
	var body io.ReadCloser
	var err error
	for attempt := 1; ; attempt++ {
		if body, err = rc.TaskEvents(ctx, taskID, 0); err == nil {
			break
		}
		if attempt >= taskWatchAttempts {
			log.Warn("task watcher: cannot reach runtimed; marking failed", "err", err.Error())
			s.finishWatchedTask(taskID, failedResult(taskID, "sandbox_unavailable",
				"task watcher could not reach runtimed"))
			return
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
	defer body.Close()

	var result *runtime.TaskResult
	_ = runtime.DecodeEvents(body, func(ev runtime.Event) bool {
		if ev.Type == runtime.EventDone {
			var tr runtime.TaskResult
			if json.Unmarshal(ev.Data, &tr) == nil {
				result = &tr
			}
			return false // terminal
		}
		return true
	})
	if result == nil {
		log.Warn("task watcher: event stream ended without a terminal event")
		s.finishWatchedTask(taskID, failedResult(taskID, "internal",
			"task event stream ended without a terminal event"))
		return
	}
	s.finishWatchedTask(taskID, result)
	log.Info("task watcher: result persisted", "status", result.Status)

	// auto-git-push: after the result is durable, push the workspace to
	// its assigned remote (host-side, best-effort). Never affects the
	// task result. No-op when the sandbox has no remote / no host token.
	s.pushOnTaskFinish(sandboxID, taskID)
}

func (s *Server) finishWatchedTask(taskID string, result *runtime.TaskResult) {
	raw, _ := json.Marshal(result)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.FinishTask(ctx, taskID, string(result.Status), string(raw)); err != nil {
		s.Log.Warn("task watcher: FinishTask failed", "task", taskID, "err", err.Error())
	}
}

// ReconcileTasks finalizes tasks left `running` by a previous sandboxd
// run (whose watcher goroutines did not survive the restart). Run once
// at boot, before the idle reaper — which trusts the task table —
// begins ticking. Per running row:
//   - runtimed already wrote result.json -> finalize from it;
//   - else the sandbox is still running -> re-attach a watcher;
//   - else -> finalize as a clean sandbox_unavailable failure.
//
// An interrupted task therefore lands on the existing failure model
// (status=failed, failure_reason=sandbox_unavailable) — no new
// terminal state is introduced.
func (s *Server) ReconcileTasks(ctx context.Context) {
	tasks, err := s.Store.ListRunningTasks(ctx)
	if err != nil {
		s.Log.Warn("task reconcile: list running tasks failed", "err", err.Error())
		return
	}
	for _, t := range tasks {
		_, mnt := s.Loopback.Paths(t.SandboxID)
		resultPath := filepath.Join(mnt, ".runtimed", "tasks", t.TaskID, "result.json")
		if raw, rerr := os.ReadFile(resultPath); rerr == nil {
			var tr runtime.TaskResult
			if json.Unmarshal(raw, &tr) == nil && tr.Status != "" {
				s.finishWatchedTask(t.TaskID, &tr)
				s.Log.Info("task reconcile: finalized from runtimed result.json",
					"task", t.TaskID, "status", tr.Status)
				continue
			}
		}
		if sb, gerr := s.Store.Get(ctx, t.SandboxID); gerr == nil && sb.Status == "running" {
			s.Log.Info("task reconcile: sandbox running — re-attaching watcher", "task", t.TaskID)
			go s.watchTask(t.SandboxID, t.TaskID)
			continue
		}
		s.finishWatchedTask(t.TaskID, failedResult(t.TaskID, "sandbox_unavailable",
			"task interrupted by a sandboxd restart; the sandbox was unavailable to resume or report it"))
		s.Log.Info("task reconcile: finalized interrupted task", "task", t.TaskID)
	}
}
