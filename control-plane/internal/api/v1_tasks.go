package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/runtime"
	"github.com/sandboxd/control-plane/internal/store"
)

// runtimeClientFor builds a runtime.Client for a sandbox's runtimed.
func (s *Server) runtimeClientFor(id string) *runtime.Client {
	_, mnt := s.Loopback.Paths(id)
	return runtime.NewClient(filepath.Join(mnt, ".runtimed", "sock"))
}

// --- POST /v1/sandboxes/{id}/tasks ----------------------------------

type v1TaskSubmitReq struct {
	Prompt string `json:"prompt"`
	Agent  string `json:"agent,omitempty"`
}

func (s *Server) v1SubmitTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// B1 — wake-on-task-submit: a stopped sandbox is woken first by
	// delegating to the proven internal wake path. (A private sandbox
	// whose wake path expects a preview-token cookie is not covered —
	// see the runtimed README "NOT implemented yet".)
	if sb.Status == "stopped" {
		code, body := s.delegate(r, s.handleWakeJSON, http.MethodPost, "/wake/"+id,
			map[string]string{"id": id}, nil)
		if code != http.StatusOK {
			relayV1Error(w, code, body) // 503 -> sandbox_capacity, etc.
			return
		}
		if sb, err = s.Store.Get(r.Context(), id); err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	if sb.Status != "running" {
		writeV1Err(w, http.StatusConflict, "conflict",
			"sandbox is "+sb.Status+" — cannot run a task")
		return
	}

	var req v1TaskSubmitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if req.Prompt == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "prompt is required")
		return
	}
	agent := req.Agent
	if agent == "" {
		agent = "opencode"
	}
	if agent != "opencode" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"only the 'opencode' agent is supported")
		return
	}

	taskID := newULID()
	if err := s.runtimeClientFor(id).StartTask(r.Context(), runtime.StartTaskRequest{
		TaskID: taskID, Prompt: req.Prompt, Agent: agent,
	}); err != nil {
		if errors.Is(err, runtime.ErrTaskInProgress) {
			writeV1Err(w, http.StatusConflict, "task_in_progress", "a task is already in progress")
			return
		}
		writeV1Err(w, http.StatusBadGateway, "sandbox_unavailable", "runtimed: "+err.Error())
		return
	}

	// B2 — persist the durable task row; B3 — start the result watcher.
	if err := s.Store.CreateTask(r.Context(), &store.Task{
		TaskID: taskID, SandboxID: id, Agent: agent, Prompt: req.Prompt,
		Status:         "running",
		ExternalUserID: sb.ExternalUserID, ExternalProjectID: sb.ExternalProjectID,
	}); err != nil {
		// The task is running in runtimed but the row failed to write.
		// The task still proceeds; GET would 404 until reconciled.
		s.loggerFor(r, id).Error("v1 task: CreateTask failed", "task", taskID, "err", err.Error())
	} else {
		go s.watchTask(id, taskID)
	}

	s.auditAction(r, audit.Entry{
		Action: "task.create", Target: id,
		Detail: map[string]any{"task_id": taskID, "agent": agent},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         taskID,
		"sandbox_id": id,
		"status":     "running",
		"agent":      agent,
		"events_url": fmt.Sprintf("/v1/sandboxes/%s/tasks/%s/events", id, taskID),
	})
}

// --- GET /v1/sandboxes/{id}/tasks/{taskId} --------------------------

// v1Task is the canonical task result: runtime.TaskResult plus the
// owning sandbox id (the embedded struct's json tags are promoted).
type v1Task struct {
	SandboxID string `json:"sandbox_id"`
	runtime.TaskResult
}

// v1GetTask reads the canonical result from sandboxd's durable task
// store — so it works whether or not the sandbox is still running, and
// after the sandbox has been destroyed.
func (s *Server) v1GetTask(w http.ResponseWriter, r *http.Request) {
	id, taskID := r.PathValue("id"), r.PathValue("taskId")
	t, err := s.Store.GetTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such task")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if t.SandboxID != id {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such task for that sandbox")
		return
	}
	if t.Status == "running" || !t.ResultJSON.Valid {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": taskID, "sandbox_id": id, "status": "running",
		})
		return
	}
	var tr runtime.TaskResult
	if err := json.Unmarshal([]byte(t.ResultJSON.String), &tr); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal",
			"decode stored task result: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v1Task{SandboxID: id, TaskResult: tr})
}

// --- GET /v1/sandboxes/{id}/tasks/{taskId}/events (SSE) -------------

func (s *Server) v1TaskEvents(w http.ResponseWriter, r *http.Request) {
	id, taskID := r.PathValue("id"), r.PathValue("taskId")
	since := 0
	if leid := r.Header.Get("Last-Event-ID"); leid != "" {
		if n, err := strconv.Atoi(leid); err == nil {
			since = n + 1 // resume after the last delivered event
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			since = n
		}
	}
	body, err := s.runtimeClientFor(id).TaskEvents(r.Context(), taskID, since)
	if err != nil {
		writeV1Err(w, http.StatusBadGateway, "sandbox_unavailable", "runtimed: "+err.Error())
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	_ = runtime.DecodeEvents(body, func(ev runtime.Event) bool {
		data := ev.Data
		if len(data) == 0 {
			data = json.RawMessage("{}")
		}
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data)
		if flusher != nil {
			flusher.Flush()
		}
		return r.Context().Err() == nil
	})
}

// --- POST /v1/sandboxes/{id}/tasks/{taskId}/cancel ------------------

func (s *Server) v1CancelTask(w http.ResponseWriter, r *http.Request) {
	id, taskID := r.PathValue("id"), r.PathValue("taskId")
	if err := s.runtimeClientFor(id).CancelTask(r.Context(), taskID); err != nil {
		writeV1Err(w, http.StatusBadGateway, "sandbox_unavailable", "runtimed: "+err.Error())
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "task.cancel", Target: id,
		Detail: map[string]any{"task_id": taskID},
	})
	writeJSON(w, http.StatusOK, map[string]string{"id": taskID, "status": "cancelling"})
}
