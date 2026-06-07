package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sandboxd/control-plane/internal/runtime"
)

var errTaskInProgress = errors.New("a task is already in progress")

const defaultTaskTimeout = 10 * time.Minute

// eventSink receives canonical events from an agent adapter.
type eventSink func(evType string, data any)

// task is one coding-agent run. One at a time per sandbox.
type task struct {
	id        string
	prompt    string
	agentName string
	env       map[string]string
	timeout   time.Duration
	dir       string // .runtimed/tasks/<id>
	createdAt time.Time

	mu        sync.Mutex
	startedAt time.Time
	events    []runtime.Event
	updatedCh chan struct{} // closed + replaced on every new event
	phase     string
	done      bool
	result    *runtime.TaskResult
	eventsW   *os.File
	cancel    context.CancelFunc
}

func newTask(req runtime.StartTaskRequest, tasksRoot string) (*task, error) {
	dir := filepath.Join(tasksRoot, req.TaskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	timeout := defaultTaskTimeout
	if req.TimeoutS > 0 {
		timeout = time.Duration(req.TimeoutS) * time.Second
	}
	return &task{
		id: req.TaskID, prompt: req.Prompt, agentName: req.Agent, env: req.Env,
		timeout: timeout, dir: dir, createdAt: time.Now().UTC(),
		updatedCh: make(chan struct{}), phase: "queued", eventsW: f,
	}, nil
}

// emit appends an event to the in-memory log and events.jsonl, then
// wakes any streamers.
func (t *task) emit(evType string, data any) {
	raw, _ := json.Marshal(data)
	t.mu.Lock()
	ev := runtime.Event{ID: len(t.events), Type: evType, Time: time.Now().UTC(), Data: raw}
	t.events = append(t.events, ev)
	if t.eventsW != nil {
		if line, err := json.Marshal(ev); err == nil {
			_, _ = t.eventsW.Write(append(line, '\n'))
		}
	}
	close(t.updatedCh)
	t.updatedCh = make(chan struct{})
	t.mu.Unlock()
}

func (t *task) setPhase(p string) {
	t.mu.Lock()
	t.phase = p
	t.mu.Unlock()
	t.emit(runtime.EventStatus, map[string]any{"phase": p})
}

// eventsSince returns events from index cursor and a channel that is
// closed when the next event arrives.
func (t *task) eventsSince(cursor int) ([]runtime.Event, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []runtime.Event
	if cursor < len(t.events) {
		out = append(out, t.events[cursor:]...)
	}
	return out, t.updatedCh
}

func (t *task) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// finish emits the terminal `done` event, persists result.json, and
// closes the event log.
func (t *task) finish(res runtime.TaskResult) {
	t.emit(runtime.EventDone, res)
	t.mu.Lock()
	t.done = true
	t.phase = "done"
	t.result = &res
	if t.eventsW != nil {
		_ = t.eventsW.Close()
		t.eventsW = nil
	}
	close(t.updatedCh)
	t.updatedCh = make(chan struct{})
	dir := t.dir
	t.mu.Unlock()
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
	}
}

// --- task manager (methods on app) ---------------------------------

func selectAgent(name string, log *slog.Logger) (agent, error) {
	if name == "" || name == "opencode" {
		return &opencodeAgent{log: log}, nil
	}
	return nil, fmt.Errorf("unsupported agent %q (this slice supports opencode only)", name)
}

// startTask enforces one active task per sandbox and launches the run.
func (a *app) startTask(req runtime.StartTaskRequest) (*task, error) {
	if _, err := selectAgent(req.Agent, a.log); err != nil {
		return nil, err
	}
	a.taskMu.Lock()
	defer a.taskMu.Unlock()
	if a.task != nil && !a.task.isDone() {
		return nil, errTaskInProgress
	}
	t, err := newTask(req, filepath.Join(a.runtimeDir, "tasks"))
	if err != nil {
		return nil, err
	}
	a.task = t
	t.emit(runtime.EventStatus, map[string]any{"phase": "starting"})
	go a.runTask(t)
	return t, nil
}

func (a *app) cancelTask(id string) {
	a.taskMu.Lock()
	t := a.task
	a.taskMu.Unlock()
	if t == nil || t.id != id {
		return
	}
	t.mu.Lock()
	c := t.cancel
	t.mu.Unlock()
	if c != nil {
		c()
	}
}

// runTask is the whole task lifecycle: checkpoint → agent → build
// check → canonical result.
func (a *app) runTask(t *task) {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	t.mu.Lock()
	t.cancel = cancel
	t.startedAt = time.Now().UTC()
	t.mu.Unlock()

	res := runtime.TaskResult{ID: t.id, CreatedAt: t.createdAt, StartedAt: t.startedAt, FilesChanged: []string{}}

	// 1. pre-task checkpoint.
	t.setPhase("checkpoint")
	checkpointID, err := checkpoint(a.appDir, t.id)
	if err != nil {
		a.log.Warn("task checkpoint failed", "task", t.id, "err", err.Error())
	}
	res.CheckpointID = checkpointID

	// 2. run the agent.
	t.setPhase("agent_running")
	ag, err := selectAgent(t.agentName, a.log)
	if err != nil {
		a.finishTask(t, &res, runtime.TaskFailed, "internal", err.Error())
		return
	}
	rawLog, _ := os.Create(filepath.Join(t.dir, "agent.log"))
	var rl io.Writer = io.Discard
	if rawLog != nil {
		rl = rawLog
		defer rawLog.Close()
	}
	finalMsg, usage, agentErr := ag.run(ctx, agentSpec{
		workDir: a.appDir, prompt: t.prompt, env: t.env, rawLog: rl,
	}, t.emit)
	res.AgentMessageFinal = finalMsg
	res.Tokens = usage

	// 3. classify the outcome.
	status, reason, errMsg := runtime.TaskSucceeded, "", ""
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status, reason, errMsg = runtime.TaskFailed, "agent_timeout", "agent did not finish within the task timeout"
	case errors.Is(ctx.Err(), context.Canceled):
		status, reason = runtime.TaskCancelled, "cancelled"
	case agentErr != nil:
		status, reason, errMsg = runtime.TaskFailed, "agent_error", agentErr.Error()
	}

	// 4. files changed — captured even on failure / cancel.
	if files, ferr := filesChanged(a.appDir, checkpointID); ferr == nil {
		res.FilesChanged = files
	} else if checkpointID != "" {
		a.log.Warn("files-changed diff failed", "task", t.id, "err", ferr.Error())
	}

	// 5. post-task build check (skipped on cancel — keep cancel fast).
	if status != runtime.TaskCancelled {
		t.setPhase("build_check")
		ok, bmsg := buildCheck(a.appDir, a.log)
		res.BuildOK = ok
		res.BuildErrorMessage = bmsg
		t.emit(runtime.EventBuild, map[string]any{"build_ok": ok, "build_error_message": bmsg})
	}

	// 5.5 post-task health pipeline (skipped on cancel): remediate
	// dev-server state the build check can't see (e.g. stale config →
	// restart) then probe the live entry assets and report a preview
	// error if the app is blank despite a clean build. Extensible — add
	// failure modes in cmd/runtimed/health.go.
	if status != runtime.TaskCancelled {
		t.setPhase("health_check")
		res.PreviewErrorMessage = a.runPostTaskChecks(ctx, res.FilesChanged)
	}

	// 6. preview state after the task — now reflects entry-asset health,
	// not just the HTML shell's 200.
	a.probe()
	res.PreviewStatusAfter = a.status().Preview.Status

	a.finishTask(t, &res, status, reason, errMsg)
}

func (a *app) finishTask(t *task, res *runtime.TaskResult, status runtime.TaskStatus, reason, errMsg string) {
	res.Status = status
	res.FailureReason = reason
	res.ErrorMessage = errMsg
	res.FinishedAt = time.Now().UTC()
	res.DurationMS = res.FinishedAt.Sub(t.startedAt).Milliseconds()
	if res.FilesChanged == nil {
		res.FilesChanged = []string{}
	}
	t.finish(*res)
	a.log.Info("task finished", "task", t.id, "status", status,
		"build_ok", res.BuildOK, "duration_ms", res.DurationMS)
}

// activeTaskRef is the GET /status active-task summary, or nil.
func (a *app) activeTaskRef() *runtime.ActiveTask {
	a.taskMu.Lock()
	t := a.task
	a.taskMu.Unlock()
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	return &runtime.ActiveTask{ID: t.id, Status: runtime.TaskRunning, Phase: t.phase}
}

// --- HTTP handlers --------------------------------------------------

func (a *app) handleStartTask(w http.ResponseWriter, r *http.Request) {
	var req runtime.StartTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if req.TaskID == "" || req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id and prompt are required"})
		return
	}
	t, err := a.startTask(req)
	if errors.Is(err, errTaskInProgress) {
		a.taskMu.Lock()
		active := ""
		if a.task != nil {
			active = a.task.id
		}
		a.taskMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "task_in_progress", "active_task_id": active})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.log.Info("task started", "task", t.id, "agent", t.agentName)
	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": t.id, "status": "running"})
}

func (a *app) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.cancelTask(id) // idempotent — 200 regardless of current state
	writeJSON(w, http.StatusOK, map[string]string{"task_id": id, "status": "cancelling"})
}

// handleTaskEvents streams the task event log as newline-delimited
// JSON, from event index ?since (default 0), live-tailing the active
// task until its terminal `done` event.
func (a *app) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since := 0
	if s := r.URL.Query().Get("since"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			since = n
		}
	}

	a.taskMu.Lock()
	t := a.task
	a.taskMu.Unlock()
	if t != nil && t.id == id {
		w.Header().Set("Content-Type", "application/x-ndjson")
		streamLive(r.Context(), w, t, since)
		return
	}

	// A past task — replay its persisted event log.
	f, err := os.Open(filepath.Join(a.runtimeDir, "tasks", id, "events.jsonl"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such task"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		if line >= since {
			_, _ = w.Write(append(sc.Bytes(), '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
		line++
	}
}

func streamLive(ctx context.Context, w http.ResponseWriter, t *task, since int) {
	flusher, _ := w.(http.Flusher)
	cursor := since
	for {
		evs, wait := t.eventsSince(cursor)
		for _, e := range evs {
			b, _ := json.Marshal(e)
			_, _ = w.Write(append(b, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
			cursor++
			if e.Type == runtime.EventDone {
				return
			}
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return
		}
	}
}

// recoverInterruptedTasks finalizes any task that has an event log but
// no result.json — i.e. one interrupted by a stop or a runtimed crash.
// An interrupted task is failed, never resumed.
func recoverInterruptedTasks(tasksRoot string, log *slog.Logger) {
	entries, err := os.ReadDir(tasksRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tasksRoot, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "result.json")); err == nil {
			continue // already finalized
		}
		if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
			continue // not a real task dir
		}
		res := runtime.TaskResult{
			ID: e.Name(), Status: runtime.TaskFailed,
			FailureReason: "sandbox_unavailable",
			ErrorMessage:  "task interrupted by a sandbox stop or runtimed restart",
			FilesChanged:  []string{},
		}
		if b, err := json.MarshalIndent(res, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
		}
		if f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			ev := runtime.Event{Type: runtime.EventDone, Time: time.Now().UTC()}
			ev.Data, _ = json.Marshal(res)
			if line, err := json.Marshal(ev); err == nil {
				_, _ = f.Write(append(line, '\n'))
			}
			_ = f.Close()
		}
		log.Warn("recovered interrupted task", "task", e.Name())
	}
}
