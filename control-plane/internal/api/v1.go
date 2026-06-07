// v1.go — the narrow public /v1 API (ops/design/v1-external-api.md).
// It is a thin translation layer over the proven internal machinery:
// sandbox create/delete delegate to the existing internal handlers;
// runtime/task state is read via runtime.Client over the workspace
// Unix socket. The internal /sandbox API is left untouched.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/runtime"
	"github.com/sandboxd/control-plane/internal/store"
)

// defaultTemplate is the one fixed snapshot variant in v1.
const defaultTemplate = "react-standard"

// --- v1 response shapes ---------------------------------------------

type v1Preview struct {
	URL               string `json:"url"`
	Status            string `json:"status"`
	LastHTTPStatus    int    `json:"last_http_status,omitempty"`
	LastCheckedAt     string `json:"last_checked_at,omitempty"`
	BuildErrorMessage string `json:"build_error_message,omitempty"`
}

type v1Sandbox struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	Preview      v1Preview `json:"preview"`
	ActiveTaskID string    `json:"active_task_id,omitempty"`
	Template     string    `json:"template"`
	GitRemoteURL string    `json:"git_remote_url,omitempty"` // the assigned push target, echoed back
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at,omitempty"`
}

// --- helpers --------------------------------------------------------

// writeV1Err emits the v1 error envelope.
func writeV1Err(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"code": errCode, "message": msg, "retryable": code == 502 || code == 503,
	}})
}

func v1ErrCode(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusServiceUnavailable:
		return "sandbox_capacity"
	default:
		return "internal"
	}
}

// relayV1Error reshapes an internal-handler error body into the v1
// envelope.
func relayV1Error(w http.ResponseWriter, code int, body []byte) {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(code)
	}
	writeV1Err(w, code, v1ErrCode(code), msg)
}

// delegate invokes an internal handler with a synthesized request and
// returns the captured status + body. The original request context is
// carried so the auth actor and request id propagate.
func (s *Server) delegate(r *http.Request, h http.HandlerFunc, method, path string, pathValues map[string]string, body []byte) (int, []byte) {
	var inner *http.Request
	if body != nil {
		inner = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		inner = httptest.NewRequest(method, path, nil)
	}
	inner = inner.WithContext(r.Context())
	for k, v := range pathValues {
		inner.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, inner)
	return rec.Code, rec.Body.Bytes()
}

func (s *Server) previewURL(id string) string {
	return fmt.Sprintf("https://s-%s-3000.preview.%s", id, s.PreviewDomain)
}

// v1SandboxFromRow reshapes a stored sandbox to the v1 object, folding
// in the live runtime/preview state from runtimed when reachable.
func (s *Server) v1SandboxFromRow(r *http.Request, sb *store.Sandbox) v1Sandbox {
	out := v1Sandbox{
		ID:        sb.ID,
		Status:    sb.Status,
		Template:  defaultTemplate,
		CreatedAt: sb.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: sb.UpdatedAt.UTC().Format(time.RFC3339),
	}
	// Echo the assigned git push target so the caller can read back the
	// platform's record of the assignment (auto-git-push).
	if remote, err := s.Store.GitRemote(r.Context(), sb.ID); err == nil {
		out.GitRemoteURL = remote
	}
	prev := v1Preview{URL: s.previewURL(sb.ID)}
	_, mnt := s.Loopback.Paths(sb.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if rs, err := runtime.NewClient(filepath.Join(mnt, ".runtimed", "sock")).Status(ctx); err == nil {
		prev.Status = string(rs.Preview.Status)
		prev.LastHTTPStatus = rs.Preview.LastHTTPStatus
		if rs.Preview.LastCheckedAt != nil {
			prev.LastCheckedAt = rs.Preview.LastCheckedAt.UTC().Format(time.RFC3339)
		}
		prev.BuildErrorMessage = rs.Preview.BuildErrorMessage
		if rs.ActiveTask != nil {
			out.ActiveTaskID = rs.ActiveTask.ID
		}
	} else if sb.Status == "running" {
		prev.Status = "starting" // running but runtimed not yet answering
	} else {
		prev.Status = "down"
	}
	out.Preview = prev
	return out
}

// --- POST /v1/sandboxes ---------------------------------------------

type v1CreateReq struct {
	Project struct {
		ID     string `json:"id"`
		UserID string `json:"user_id"`
	} `json:"project"`
	Visibility string `json:"visibility,omitempty"`
	Template   string `json:"template,omitempty"`
	// FromSnapshot, when set, clones the new sandbox's workspace from a
	// snapshot the caller's tenant owns (ops/design/snapshots-as-templates.md)
	// instead of the default template. Mutually exclusive with Template.
	FromSnapshot string `json:"from_snapshot,omitempty"`
	// GitRemoteURL, when set, makes the platform push this app's
	// workspace to that https git remote on each task finish. The repo
	// the caller assigns here is echoed back on every Sandbox object so
	// the caller can read the platform's record of the assignment.
	GitRemoteURL string `json:"git_remote_url,omitempty"`
}

func (s *Server) v1CreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req v1CreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if req.Project.ID == "" || req.Project.UserID == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "project.id and project.user_id are required")
		return
	}

	// Idempotent per project: an existing non-error sandbox for this
	// project is returned as-is (one durable sandbox per project).
	if rows, err := s.Store.ListFiltered(r.Context(), "", req.Project.ID); err == nil {
		for _, sb := range rows {
			if sb.Status != "error" {
				writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
				return
			}
		}
	}

	if req.FromSnapshot != "" && req.Template != "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "template and from_snapshot are mutually exclusive")
		return
	}
	vis := req.Visibility
	if vis == "" {
		vis = "public"
	}
	createBody := map[string]any{
		"ports":      []int{3000},
		"visibility": vis,
		"external":   map[string]string{"user_id": req.Project.UserID, "project_id": req.Project.ID},
	}
	if req.GitRemoteURL != "" {
		createBody["git_remote_url"] = req.GitRemoteURL
	}
	if req.FromSnapshot != "" {
		// Resolve + authorize the snapshot under the caller's tenant,
		// then hand handleCreate the pre-resolved image path. A
		// cross-tenant or missing snapshot is a 404 (don't leak existence).
		snap, err := s.snapshotForTenant(r, req.FromSnapshot)
		if errors.Is(err, store.ErrNotFound) {
			writeV1Err(w, http.StatusNotFound, "not_found", "no such snapshot")
			return
		}
		if err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if snap.Status != "ready" {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "snapshot is not ready")
			return
		}
		createBody["template_path"] = snap.ImagePath
	} else {
		tmpl := req.Template
		if tmpl == "" {
			tmpl = defaultTemplate
		}
		createBody["template"] = tmpl
	}
	internal, _ := json.Marshal(createBody)
	code, body := s.delegate(r, s.handleCreate, http.MethodPost, "/sandbox", nil, internal)
	if code != http.StatusCreated {
		// Capacity pushback (503) carries a Retry-After on the internal
		// handler; the delegate's recorder drops it, so re-assert it here
		// to honour the documented backpressure contract (llm.txt §3/§5.1).
		if code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		relayV1Error(w, code, body)
		return
	}
	var sr sandboxResp
	if err := json.Unmarshal(body, &sr); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "decode create result: "+err.Error())
		return
	}
	sb, err := s.Store.Get(r.Context(), sr.ID)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "post-create get: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.v1SandboxFromRow(r, sb))
}

// --- GET /v1/sandboxes/{id} -----------------------------------------

func (s *Server) v1GetSandbox(w http.ResponseWriter, r *http.Request) {
	sb, err := s.Store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such sandbox")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
}

// --- POST /v1/sandboxes/{id}/stop -----------------------------------

func (s *Server) v1StopSandbox(w http.ResponseWriter, r *http.Request) {
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
	if sb.Status == "stopped" {
		writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb)) // idempotent
		return
	}
	if sb.Status != "running" {
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is not running")
		return
	}
	// Reject a stop while a task is active — the upstream cancels first.
	_, mnt := s.Loopback.Paths(id)
	rctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if rs, rerr := runtime.NewClient(filepath.Join(mnt, ".runtimed", "sock")).Status(rctx); rerr == nil && rs.ActiveTask != nil {
		writeV1Err(w, http.StatusConflict, "task_in_progress",
			"a task is in progress; cancel it before stopping")
		return
	}
	if err := s.Docker.Stop(r.Context(), "s-"+id, 10); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "docker stop: "+err.Error())
		return
	}
	if err := s.Store.MarkStoppedAt(r.Context(), id, time.Now().UTC()); err != nil {
		s.loggerFor(r, id).Warn("v1 stop: MarkStoppedAt failed", "err", err.Error())
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.stop", Target: id})
	sb, _ = s.Store.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
}

// --- DELETE /v1/sandboxes/{id} --------------------------------------

// v1DeleteSandbox is a full destroy. It delegates to the internal
// purge (container + workspace .img + row), not the internal soft
// DELETE — the soft DELETE preserves the .img for id-reuse, which is
// not the v1 "destroy the project's sandbox" contract.
func (s *Server) v1DeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	code, body := s.delegate(r, s.handlePurgeSandbox, http.MethodPost, "/sandbox/"+id+"/purge",
		map[string]string{"id": id}, nil)
	if code >= 200 && code < 300 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	relayV1Error(w, code, body)
}
