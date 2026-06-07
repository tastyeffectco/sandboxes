package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/cgroup"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/logging"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/runtime"
	"github.com/sandboxd/control-plane/internal/snapshot"
	"github.com/sandboxd/control-plane/internal/store"
	"github.com/sandboxd/control-plane/internal/traefik"
	"github.com/sandboxd/control-plane/internal/wake"
)

// --- request / response payloads ------------------------------------

// externalReq is the Phase 8 upstream-identity object on POST /sandbox.
type externalReq struct {
	UserID      string `json:"user_id"`
	ProjectID   string `json:"project_id"`
	WorkspaceID string `json:"workspace_id"`
}

type createReq struct {
	ID         string      `json:"id,omitempty"`
	Ports      []int       `json:"ports,omitempty"`
	MemoryHigh string      `json:"memory_high,omitempty"`
	Visibility string      `json:"visibility,omitempty"`
	External   externalReq `json:"external"`
	// Template, when set, is the name of a prebuilt golden template
	// .img to reflink-clone the workspace from instead of an empty
	// seeded provision (ops/design/fast-coldstart-react-vite-snapshot.md
	// — the fast-cold-start path). Empty = the existing scaffold-at-
	// runtime behaviour, unchanged.
	Template string `json:"template,omitempty"`
	// TemplatePath is an INTERNAL, pre-resolved absolute path to a
	// golden .img to clone from — set by the v1 snapshot spin-up path
	// (ops/design/snapshots-as-templates.md) after it has resolved and
	// authorized the snapshot. It must live under TemplatesDir or
	// LibraryRoot (validated below). Mutually exclusive with Template.
	// Not part of the public /v1 contract; never set from an external
	// body in practice (v1 builds the internal body itself).
	TemplatePath string `json:"template_path,omitempty"`
	// GitRemoteURL, when set, is the https git remote sandboxd pushes
	// the app workspace to on each task finish (auto-git-push). The URL
	// is not a secret; the master token is host-side. Empty = off.
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	// Env injects environment variables into the sandbox container at
	// create time (e.g. {"ANTHROPIC_API_KEY":"sk-..."}). They are visible
	// to the container's main process (runtimed) and therefore to coding
	// agents (opencode/claude) it spawns. Values are passed straight to
	// `docker run --env`; keys must be non-empty and free of '=' and
	// newlines.
	Env map[string]string `json:"env,omitempty"`
}

type sandboxResp struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	Image         string  `json:"image"`
	WorkspaceImg  string  `json:"workspace_img"`
	WorkspaceMnt  string  `json:"workspace_mnt"`
	ContainerID   string  `json:"container_id,omitempty"`
	CgroupPath    string  `json:"cgroup_path,omitempty"`
	MemoryHigh    string  `json:"memory_high"`
	ErrorMessage  string  `json:"error_message,omitempty"`
	Ports         []int   `json:"ports"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	// Phase 5 — surface the activity columns so the roadmap §Validation
	// V2/V3 expressions (`jq .row.last_active_at`, `jq .row.status`)
	// work directly. last_active_at and stopped_at are unix seconds;
	// keepalive_until is unix seconds or 0 when unset.
	LastActiveAt   int64 `json:"last_active_at"`
	StoppedAt      int64 `json:"stopped_at,omitempty"`
	KeepaliveUntil int64 `json:"keepalive_until,omitempty"`
	// Phase 6 — container bridge IP, so roadmap §Validation V2
	// (`jq .row.container_ip`) works directly. Empty string while
	// the sandbox is stopped.
	ContainerIP string `json:"container_ip"`
	// Phase 8 — external identity passthrough + visibility.
	ExternalUserID      string `json:"external_user_id,omitempty"`
	ExternalProjectID   string `json:"external_project_id,omitempty"`
	ExternalWorkspaceID string `json:"external_workspace_id,omitempty"`
	Visibility          string `json:"visibility"`
}

type getResp struct {
	Row       sandboxResp `json:"row"`
	LiveState any         `json:"live_state"`
	// Runtime is the in-sandbox runtimed snapshot (preview state,
	// active task), or null when runtimed is unreachable (sandbox
	// stopped, or not yet booted).
	Runtime *runtime.Status `json:"runtime,omitempty"`
}

type execReq struct {
	Cmd    []string `json:"cmd"`
	Stream bool     `json:"stream,omitempty"`
}

type execResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func toRespRow(sb *store.Sandbox) sandboxResp {
	r := sandboxResp{
		ID:           sb.ID,
		Status:       sb.Status,
		Image:        sb.Image,
		WorkspaceImg: sb.WorkspaceImg,
		WorkspaceMnt: sb.WorkspaceMnt,
		MemoryHigh:   sb.MemoryHigh,
		Ports:        sb.Ports,
		CreatedAt:    sb.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    sb.UpdatedAt.Format(time.RFC3339),
	}
	if sb.ContainerID.Valid {
		r.ContainerID = sb.ContainerID.String
	}
	if sb.CgroupPath.Valid {
		r.CgroupPath = sb.CgroupPath.String
	}
	if sb.ErrorMessage.Valid {
		r.ErrorMessage = sb.ErrorMessage.String
	}
	if !sb.LastActiveAt.IsZero() {
		r.LastActiveAt = sb.LastActiveAt.Unix()
	}
	if sb.StoppedAt.Valid {
		r.StoppedAt = sb.StoppedAt.Int64
	}
	if sb.KeepaliveUntil.Valid {
		r.KeepaliveUntil = sb.KeepaliveUntil.Int64
	}
	if sb.ContainerIP.Valid {
		r.ContainerIP = sb.ContainerIP.String
	}
	if sb.ExternalUserID.Valid {
		r.ExternalUserID = sb.ExternalUserID.String
	}
	if sb.ExternalProjectID.Valid {
		r.ExternalProjectID = sb.ExternalProjectID.String
	}
	if sb.ExternalWorkspaceID.Valid {
		r.ExternalWorkspaceID = sb.ExternalWorkspaceID.String
	}
	r.Visibility = sb.Visibility
	if r.Visibility == "" {
		r.Visibility = "public"
	}
	return r
}

// --- helpers --------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// auditAction records one privileged-action audit row, filling the
// actor fields from the request context (set by the auth middleware).
// nil-safe — a Server built without an audit logger is a no-op.
func (s *Server) auditAction(r *http.Request, e audit.Entry) {
	if s.Audit == nil {
		return
	}
	a := auth.ActorFrom(r.Context())
	e.ActorKind = a.Kind
	e.ActorName = a.Name
	e.ActorIP = a.IP
	s.Audit.Write(r.Context(), e)
}

// validExternalID enforces the roadmap §4 constraint on the opaque
// upstream identifier strings: len ≤ 256, no control codes, no commas
// (we use these values in unstructured / comma-joined contexts). An
// empty string is valid — only external.user_id is required, and that
// is checked separately by the caller.
func validExternalID(s string) bool {
	if len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c == ',' {
			return false
		}
	}
	return true
}

// nullStr maps "" to a NULL sql.NullString.
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// validTemplateName guards the optional `template` field on
// POST /sandbox. A template name is resolved to a host-local file
// <TemplatesDir>/<name>.img, so it must be a plain identifier — no
// path separators, no traversal, no dots. New variants
// (ops/design/fast-coldstart-react-vite-snapshot.md §4) are added as
// new .img files, so no allowlist is hardcoded here — a new variant
// needs no code change, only the golden .img on the host.
func validTemplateName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// pathUnderRoot reports whether clean (an already filepath.Clean'd
// path) is the root itself or a descendant of it. root "" never
// matches. Both sides are cleaned so a trailing slash on root is fine.
func pathUnderRoot(clean, root string) bool {
	if root == "" {
		return false
	}
	root = filepath.Clean(root)
	return clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))
}

func (s *Server) loggerFor(r *http.Request, sandboxID string) interface {
	Info(string, ...any)
	Warn(string, ...any)
	Error(string, ...any)
} {
	rid := logging.RequestID(r.Context())
	l := s.Log.With("request_id", rid)
	if sandboxID != "" {
		l = l.With("sandbox_id", sandboxID)
	}
	return l
}

// newULID generates a ULID using crypto/rand so multiple processes
// don't collide on the monotonic entropy source.
func newULID() string {
	t := time.Now()
	return ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

func isULID(s string) bool {
	_, err := ulid.Parse(s)
	return err == nil
}

// --- POST /sandbox --------------------------------------------------

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.MemoryHigh == "" {
		req.MemoryHigh = "4G"
	}
	for _, p := range req.Ports {
		if p < 1 || p > 65535 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("port out of range: %d", p))
			return
		}
	}
	if req.ID == "" {
		req.ID = newULID()
	} else if !isULID(req.ID) {
		writeErr(w, http.StatusBadRequest, "id must be a ULID")
		return
	}
	log := s.loggerFor(r, req.ID)

	// External identity tags a sandbox with the tenant/user it belongs to
	// (used for ownership guards on id-reuse and for private-sandbox auth).
	// The upstream backend always sets it; for the OSS single-operator
	// quickstart it defaults to "local" so `POST /sandbox {"ports":[...]}`
	// works with no body bookkeeping.
	if req.External.UserID == "" {
		req.External.UserID = "local"
	}
	for _, f := range []struct{ label, val string }{
		{"external.user_id", req.External.UserID},
		{"external.project_id", req.External.ProjectID},
		{"external.workspace_id", req.External.WorkspaceID},
	} {
		if !validExternalID(f.val) {
			writeErr(w, http.StatusBadRequest,
				"invalid "+f.label+": must be <=256 chars, no control codes, no commas")
			return
		}
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = "public"
	}
	if visibility != "public" && visibility != "private" {
		writeErr(w, http.StatusBadRequest, "visibility must be 'public' or 'private'")
		return
	}
	if req.GitRemoteURL != "" && !strings.HasPrefix(req.GitRemoteURL, "https://") {
		writeErr(w, http.StatusBadRequest, "git_remote_url must be an https:// URL")
		return
	}

	// Optional fast-cold-start template
	// (ops/design/fast-coldstart-react-vite-snapshot.md). When set, the
	// workspace .img is a reflink (CoW) clone of a prebuilt golden
	// template instead of an empty provision+seed — zero install, zero
	// scaffold, zero network on the create hot path. Resolve and
	// validate the golden .img up front so an unknown/unavailable
	// template fails cleanly with 400/503 before any row is created.
	var templatePath string
	switch {
	case req.TemplatePath != "" && req.Template != "":
		writeErr(w, http.StatusBadRequest, "template and template_path are mutually exclusive")
		return
	case req.TemplatePath != "":
		// Pre-resolved snapshot spin-up path. Defence in depth: the path
		// is built by the v1 layer from a snapshot row we wrote, but we
		// still require it to live under an allowed root so a bug can
		// never clone an arbitrary host file into a workspace.
		clean := filepath.Clean(req.TemplatePath)
		if !pathUnderRoot(clean, s.LibraryRoot) && !pathUnderRoot(clean, s.TemplatesDir) {
			writeErr(w, http.StatusBadRequest, "template_path outside allowed roots")
			return
		}
		if _, err := os.Stat(clean); err != nil {
			writeErr(w, http.StatusBadRequest, "template image unavailable")
			return
		}
		templatePath = clean
	case req.Template != "":
		if !validTemplateName(req.Template) {
			writeErr(w, http.StatusBadRequest,
				"invalid template: must be lowercase [a-z0-9-], <=64 chars")
			return
		}
		if s.TemplatesDir == "" {
			writeErr(w, http.StatusServiceUnavailable,
				"templates not configured on this host (SANDBOXD_TEMPLATES_DIR unset)")
			return
		}
		templatePath = filepath.Join(s.TemplatesDir, req.Template+".img")
		if _, err := os.Stat(templatePath); err != nil {
			writeErr(w, http.StatusBadRequest,
				"unknown or unavailable template: "+req.Template)
			return
		}
	}

	// Phase 5 — wake admission check applies to brand-new creates too
	// (CLAUDE.md "Wake admission" floor is uniform across wake and
	// create). Only enforced when Admit has been initialised in main;
	// nil Tick is fine — Admit handles a missing tick gracefully.
	if s.Admit.FloorPct > 0 {
		out, aerr := wake.Admit(r.Context(), s.Admit)
		if aerr != nil {
			log.Warn("create: admit read failed (continuing)", "err", aerr.Error())
		} else if !out.Admit {
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":                 out.Reason,
				"mem_available_percent": out.AvailPct,
			})
			return
		}
	}

	// Reject if a row already exists for this id. An .img on disk
	// without a row is the supported id-reuse path.
	if _, err := s.Store.Get(r.Context(), req.ID); err == nil {
		writeErr(w, http.StatusConflict, "sandbox row already exists for this id; DELETE first")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "get: "+err.Error())
		return
	}

	// Phase 8 §5 — id-reuse ownership guard. If a durable
	// workspace_owner row exists for this id (the .img is being
	// reattached), the caller's external.user_id MUST match it. This
	// prevents accidental cross-user resurrection of a recycled id.
	if wo, err := s.Store.GetWorkspaceOwner(r.Context(), req.ID); err == nil {
		if wo.ExternalUserID != req.External.UserID {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":                     "workspace_owner_mismatch",
				"expected_external_user_id": wo.ExternalUserID,
			})
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "workspace_owner lookup: "+err.Error())
		return
	}

	imgPath, mntPath := s.Loopback.Paths(req.ID)
	sb := &store.Sandbox{
		ID:                  req.ID,
		Status:              "creating",
		Image:               s.Image,
		WorkspaceImg:        imgPath,
		WorkspaceMnt:        mntPath,
		MemoryHigh:          req.MemoryHigh,
		Ports:               req.Ports,
		Visibility:          visibility,
		ExternalUserID:      nullStr(req.External.UserID),
		ExternalProjectID:   nullStr(req.External.ProjectID),
		ExternalWorkspaceID: nullStr(req.External.WorkspaceID),
	}
	if err := s.Store.Create(r.Context(), sb); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "id collision (race)")
			return
		}
		writeErr(w, http.StatusInternalServerError, "store.Create: "+err.Error())
		return
	}
	_ = metrics.RefreshSandboxGauge(r.Context(), s.Store)

	// Record the optional git push target (auto-git-push). Non-fatal:
	// a failure here must not abort an otherwise-good create.
	if req.GitRemoteURL != "" {
		if err := s.Store.SetGitRemote(r.Context(), req.ID, req.GitRemoteURL); err != nil {
			log.Warn("create: SetGitRemote failed", "err", err.Error())
		}
	}

	// From here on, any error path also marks the row as 'error' and
	// attempts best-effort cleanup of the half-built state.
	abort := func(msg string) {
		log.Error("create: aborting", "reason", msg)
		_ = s.Docker.Remove(context.Background(), "s-"+req.ID)
		_ = s.Loopback.Release(context.Background(), req.ID)
		_ = s.Store.MarkError(r.Context(), req.ID, msg)
		_ = metrics.RefreshSandboxGauge(r.Context(), s.Store)
	}

	// 1. Loopback provision (idempotent; reuses any existing .img).
	//    With a template, the workspace .img is a reflink clone of the
	//    golden template — the Phase 2 empty-mount seed gate stays
	//    closed because the clone is already populated. Without one,
	//    the existing empty-provision + seed path is unchanged.
	var provErr error
	if templatePath != "" {
		provErr = s.Loopback.ProvisionFromTemplate(r.Context(), req.ID, templatePath)
	} else {
		provErr = s.Loopback.Provision(r.Context(), req.ID)
	}
	if provErr != nil {
		abort("loopback provision: " + provErr.Error())
		writeErr(w, http.StatusInternalServerError, "loopback: "+provErr.Error())
		return
	}

	// 1b. Reset git when spun from a SNAPSHOT (a byte-for-byte copy of
	//     another app's workspace). The clone carries the source app's
	//     .git — its full commit history AND an `origin` pointing at the
	//     source user's repo. Leaving it would link a new user's app to
	//     another user's repo/history (and push that history onward if
	//     this app later gets its own remote). Remove it so the app
	//     starts ownerless; runtimed re-inits a clean baseline repo on
	//     the first task. Scoped to snapshots (LibraryRoot); the golden
	//     template path is untouched. Best-effort: a failure here must
	//     not abort the create.
	if s.LibraryRoot != "" && pathUnderRoot(filepath.Clean(templatePath), s.LibraryRoot) {
		gitDir := filepath.Join(mntPath, "workspace", "app", ".git")
		if err := os.RemoveAll(gitDir); err != nil {
			log.Warn("create: reset snapshot .git failed", "err", err.Error())
		} else {
			log.Info("create: reset .git on snapshot spin-up (clean ownerless repo)")
		}
	}

	// Build the optional env injection (e.g. agent API keys). Validate
	// keys so a bad entry can't smuggle extra docker flags or break the
	// KEY=VALUE encoding.
	var envFlags []string
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\n\r") || strings.ContainsAny(v, "\n\r") {
			writeErr(w, http.StatusBadRequest, "invalid env var name/value: "+k)
			return
		}
		envFlags = append(envFlags, k+"="+v)
	}

	// 2. docker run with the locked flag set + traefik labels.
	labels := traefik.Labels(req.ID, req.Ports, s.PreviewDomain, visibility, s.PreviewEntrypoint, s.PreviewTLS)
	startRun := time.Now()
	var runErr error
	containerID, runErr := s.Docker.Run(r.Context(), docker.RunSpec{
		Name:        "s-" + req.ID,
		Hostname:    "s-" + req.ID,
		Network:     s.Network,
		Userns:      s.Userns,
		ReadOnly:    true,
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		CPUShares:   100,
		Memory:      "10g",
		MemorySwap:  "10g",
		PidsLimit:   1024,
		Ulimits:     []string{"nofile=65536:65536"},
		Tmpfs:       []string{"/tmp:size=512m", "/var/tmp:size=128m"},
		Env:         envFlags,
		Volumes:     []string{mntPath + ":/home/sandbox"},
		Labels:      labels,
		Image:       s.Image,
	})
	metrics.ObserveDocker("run", startRun, &runErr)
	if runErr != nil {
		abort("docker.Run: " + runErr.Error())
		writeErr(w, http.StatusInternalServerError, "docker run: "+runErr.Error())
		return
	}

	// 3. Inspect for PID, then write memory.high.
	cj, err := s.Docker.Inspect(r.Context(), "s-"+req.ID)
	if err != nil {
		abort("docker.Inspect post-run: " + err.Error())
		writeErr(w, http.StatusInternalServerError, "inspect: "+err.Error())
		return
	}
	// memory.high is a soft cgroup-v2 throttle written directly to the
	// host cgroup fs. In the portable OSS build it is OFF by default
	// (SetMemoryHigh=false) because it needs host cgroup access the
	// control-plane container may not have; the hard --memory=10g
	// ceiling from the RunSpec still applies. When enabled but the write
	// fails, we log and continue rather than failing the whole create —
	// a missing soft throttle must not block a working sandbox.
	var rel string
	if s.SetMemoryHigh {
		rel, err = cgroup.SetMemoryHigh(r.Context(), cj.State.Pid, req.MemoryHigh)
		if err != nil {
			log.Warn("create: cgroup.SetMemoryHigh failed (continuing; --memory ceiling still applies)",
				"err", err.Error())
			rel = ""
		}
	}

	// 4. Mark running.
	if err := s.Store.MarkRunning(r.Context(), req.ID, containerID, rel); err != nil {
		abort("store.MarkRunning: " + err.Error())
		writeErr(w, http.StatusInternalServerError, "mark running: "+err.Error())
		return
	}

	// 4b. Phase 6 — record the container's bridge IP and add it to
	// the sandbox_sources_v4 nftables set BEFORE returning success.
	// roadmap §4 / §"Risks" hard rule: a sandbox running with no
	// egress rules attached is the foot-gun this section exists to
	// prevent. If nft fails, abort the row to status='error'.
	if s.Egress != nil {
		ip := cj.BridgeIP()
		if ip == "" {
			abort("docker inspect: no bridge IP available for egress policy")
			writeErr(w, http.StatusInternalServerError, "no bridge IP")
			return
		}
		if err := s.Egress.Add(r.Context(), req.ID, ip); err != nil {
			abort("egress.Add: " + err.Error())
			writeErr(w, http.StatusInternalServerError, "egress: "+err.Error())
			return
		}
		if err := s.Store.SetContainerIP(r.Context(), req.ID, ip); err != nil {
			log.Warn("create: SetContainerIP failed (continuing; in-memory map already updated)",
				"err", err.Error())
		}
	}

	// Seed last_active_at to "now" so a freshly-created sandbox isn't
	// instantly a candidate of the idle reaper before any traffic
	// arrives. Idempotent — BumpLastActive only moves forward.
	_ = s.Store.BumpLastActive(r.Context(), req.ID, time.Now().UTC())
	_ = metrics.RefreshSandboxGauge(r.Context(), s.Store)

	// Refresh the row for the response so the caller gets the
	// container_id + cgroup_path + 'running' status without a
	// follow-up GET.
	fresh, err := s.Store.Get(r.Context(), req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "post-create get: "+err.Error())
		return
	}
	createDetail := map[string]any{"visibility": visibility, "ports": req.Ports}
	if req.Template != "" {
		createDetail["template"] = req.Template
	}
	s.auditAction(r, audit.Entry{
		Action:         "sandbox.create",
		Target:         req.ID,
		ExternalUserID: req.External.UserID,
		Detail:         createDetail,
	})
	writeJSON(w, http.StatusCreated, toRespRow(fresh))
}

// --- GET /sandbox/{id} ----------------------------------------------

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no sandbox row for that id")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get: "+err.Error())
		return
	}
	resp := getResp{Row: toRespRow(sb), LiveState: nil}
	startInspect := time.Now()
	var inspectErr error
	cj, inspectErr := s.Docker.Inspect(r.Context(), "s-"+id)
	metrics.ObserveDocker("inspect", startInspect, &inspectErr)
	if inspectErr == nil {
		resp.LiveState = cj
	} else if !errors.Is(inspectErr, docker.ErrNotFound) {
		// Real docker error (not "no such container") — log but
		// don't fail the read.
		s.loggerFor(r, id).Warn("get: docker inspect failed", "err", inspectErr.Error())
	}

	// Surface in-sandbox runtime state (runtimed) over its Unix socket
	// on the workspace loopback. Best-effort: a stopped sandbox or a
	// not-yet-booted runtimed simply yields no runtime block.
	_, mnt := s.Loopback.Paths(id)
	rctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if rs, rerr := runtime.NewClient(filepath.Join(mnt, ".runtimed", "sock")).Status(rctx); rerr == nil {
		resp.Runtime = rs
	} else {
		s.loggerFor(r, id).Info("get: runtimed status unavailable", "err", rerr.Error())
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /sandboxes -------------------------------------------------

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	// Phase 8 — optional external-identity filters. When neither is
	// supplied this is the unfiltered Phase 4 listing.
	euid := r.URL.Query().Get("external_user_id")
	epid := r.URL.Query().Get("external_project_id")
	var rows []*store.Sandbox
	var err error
	if euid != "" || epid != "" {
		rows, err = s.Store.ListFiltered(r.Context(), euid, epid)
	} else {
		rows, err = s.Store.List(r.Context())
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list: "+err.Error())
		return
	}
	out := make([]sandboxResp, 0, len(rows))
	for _, sb := range rows {
		out = append(out, toRespRow(sb))
	}
	writeJSON(w, http.StatusOK, out)
}

// --- DELETE /sandbox/{id} -------------------------------------------

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Phase 7 — hold the per-id lock for the whole destroy so a
	// concurrent snapshot/restore/wake of the same id can't race the
	// container teardown + loopback release.
	if s.Locks != nil {
		s.Locks.Lock(id)
		defer s.Locks.Unlock(id)
	}
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no sandbox row for that id")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "get: "+err.Error())
		return
	}
	// Phase 6 — drop the container's bridge IP from the egress
	// sources set before tearing down the container. Removing after
	// docker rm would leave a (brief) window where the rule is gone
	// but the container could still send packets. Order matters.
	if s.Egress != nil && sb.ContainerIP.Valid {
		if err := s.Egress.Remove(r.Context(), id, sb.ContainerIP.String); err != nil {
			s.loggerFor(r, id).Warn("delete: egress.Remove failed (continuing)",
				"err", err.Error())
		}
	}
	// Cardinality hygiene: drop any per-sandbox metric label values
	// before the row goes away. The gauge will simply stop reporting
	// for this id rather than carry a stale series forever.
	metrics.EgressConnections.DeleteLabelValues(id, "http")
	metrics.EgressConnections.DeleteLabelValues(id, "https")
	metrics.EgressConnections.DeleteLabelValues(id, "ssh")
	metrics.EgressConnections.DeleteLabelValues(id, "other")

	startRm := time.Now()
	var rmErr error
	rmErr = s.Docker.Remove(r.Context(), "s-"+id)
	metrics.ObserveDocker("rm", startRm, &rmErr)
	if rmErr != nil {
		writeErr(w, http.StatusInternalServerError, "docker rm: "+rmErr.Error())
		return
	}
	if err := s.Loopback.Release(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "loopback release: "+err.Error())
		return
	}
	if err := s.Store.Delete(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "store delete: "+err.Error())
		return
	}
	_ = metrics.RefreshSandboxGauge(r.Context(), s.Store)
	extUID := ""
	if sb.ExternalUserID.Valid {
		extUID = sb.ExternalUserID.String
	}
	s.auditAction(r, audit.Entry{
		Action:         "sandbox.destroy",
		Target:         id,
		ExternalUserID: extUID,
	})
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /sandbox/{id}/exec ----------------------------------------

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(req.Cmd) == 0 {
		writeErr(w, http.StatusBadRequest, "cmd required")
		return
	}
	// Phase 8 — audit the exec recording ONLY the first argument
	// (roadmap §12: never log full command lines).
	s.auditAction(r, audit.Entry{
		Action: "sandbox.exec",
		Target: id,
		Detail: map[string]any{"cmd0": req.Cmd[0]},
	})
	// Phase 5 — count this exec as live activity. Enter on entry,
	// Exit on every return path (defer). Last-active bump at start
	// AND end so a long exec keeps the sandbox warm for the duration
	// AND for the post-exit grace window.
	if s.Inflight != nil {
		s.Inflight.Enter(id)
		defer s.Inflight.Exit(id)
	}
	now := time.Now().UTC()
	_ = s.Store.BumpLastActive(r.Context(), id, now)
	defer func() {
		_ = s.Store.BumpLastActive(r.Context(), id, time.Now().UTC())
	}()
	if req.Stream {
		// Streaming path: 200 + chunked. We invoke docker exec with
		// stdout/stderr piped to the response writer.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		startExec := time.Now()
		var execErr error
		res, execErr := s.Docker.Exec(r.Context(), "s-"+id, req.Cmd)
		metrics.ObserveDocker("exec", startExec, &execErr)
		if execErr != nil {
			_, _ = w.Write([]byte("internal-error: " + execErr.Error() + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		_, _ = w.Write([]byte(res.Stdout))
		if strings.TrimSpace(res.Stderr) != "" {
			_, _ = w.Write([]byte("---stderr---\n"))
			_, _ = w.Write([]byte(res.Stderr))
		}
		_, _ = fmt.Fprintf(w, "exit_code: %d\n", res.ExitCode)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	startExec := time.Now()
	var execErr error
	res, execErr := s.Docker.Exec(r.Context(), "s-"+id, req.Cmd)
	metrics.ObserveDocker("exec", startExec, &execErr)
	if execErr != nil {
		writeErr(w, http.StatusInternalServerError, "docker exec: "+execErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, execResp{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode})
}

// --- POST /sandbox/{id}/keepalive -----------------------------------

type keepaliveReq struct {
	Until int64 `json:"until"` // unix seconds
}

func (s *Server) handleKeepalive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.Get(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no sandbox row for that id")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "get: "+err.Error())
		return
	}
	var req keepaliveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	now := time.Now()
	max := s.KeepaliveMax
	if max <= 0 {
		max = 24 * time.Hour
	}
	until := time.Unix(req.Until, 0).UTC()
	cap := now.Add(max).UTC()
	if until.After(cap) {
		until = cap
	}
	if until.Before(now) {
		writeErr(w, http.StatusBadRequest, "until must be in the future")
		return
	}
	if err := s.Store.SetKeepaliveUntil(r.Context(), id, until); err != nil {
		writeErr(w, http.StatusInternalServerError, "set keepalive: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              id,
		"keepalive_until": until.Unix(),
	})
}

// --- POST /wake/{id} (JSON) -----------------------------------------

func (s *Server) handleWakeJSON(w http.ResponseWriter, r *http.Request) {
	if s.Wake == nil {
		writeErr(w, http.StatusServiceUnavailable, "wake handler not wired")
		return
	}
	s.Wake.ServeJSON(w, r)
}

// --- POST /sandbox/{id}/snapshots -----------------------------------

// handleSnapshotTake takes a manual snapshot. roadmap §9: allowed
// whenever the on-disk .img exists, regardless of whether a DB row
// exists; the only rejection is a row with status='running' (409).
func (s *Server) handleSnapshotTake(w http.ResponseWriter, r *http.Request) {
	if s.Snapshot == nil {
		writeErr(w, http.StatusServiceUnavailable, "snapshot subsystem not wired")
		return
	}
	id := r.PathValue("id")
	meta, err := s.Snapshot.Take(r.Context(), id, false)
	if errors.Is(err, snapshot.ErrRunning) {
		writeErr(w, http.StatusConflict, "sandbox is running; DELETE it (preserves the .img) before snapshotting")
		return
	}
	if errors.Is(err, snapshot.ErrNoImg) {
		writeErr(w, http.StatusNotFound, "no workspace .img on disk for that id")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "snapshot: "+err.Error())
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "sandbox.snapshot.create",
		Target: id,
		Detail: map[string]any{"ts": meta.TS},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ts":         meta.TS,
		"size_bytes": meta.CompressedSizeBytes,
	})
}

// --- GET /sandbox/{id}/snapshots ------------------------------------

// handleSnapshotList lists snapshots on disk. roadmap §9: operates
// against the snapshot directory directly, does NOT require a row,
// returns an empty array if the directory is absent or empty.
func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	if s.Snapshot == nil {
		writeErr(w, http.StatusServiceUnavailable, "snapshot subsystem not wired")
		return
	}
	id := r.PathValue("id")
	entries, err := s.Snapshot.List(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "snapshot list: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// --- POST /sandbox/{id}/restore -------------------------------------

type restoreReq struct {
	Snapshot string `json:"snapshot"`
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	if s.Snapshot == nil {
		writeErr(w, http.StatusServiceUnavailable, "snapshot subsystem not wired")
		return
	}
	id := r.PathValue("id")
	var req restoreReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Snapshot == "" {
		writeErr(w, http.StatusBadRequest, "snapshot timestamp required")
		return
	}
	res, err := s.Snapshot.Restore(r.Context(), id, req.Snapshot)
	if errors.Is(err, snapshot.ErrRunning) {
		writeErr(w, http.StatusConflict, "sandbox is running; DELETE it (preserves the .img) before restoring")
		return
	}
	if errors.Is(err, snapshot.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such snapshot for that id")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "restore: "+err.Error())
		return
	}
	s.auditAction(r, audit.Entry{
		Action: "sandbox.snapshot.restore",
		Target: id,
		Detail: map[string]any{"snapshot": req.Snapshot},
	})
	writeJSON(w, http.StatusOK, res)
}

// --- GET /healthz ---------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// --- GET /readyz ----------------------------------------------------

// readyz checks (a) SQLite is open and (b) `docker info` succeeded
// recently. CLAUDE.md control-plane scope explicitly: "/readyz: 200
// only if SQLite is open and `docker info` succeeded recently."
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DB().PingContext(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "sqlite ping: "+err.Error())
		return
	}
	startInfo := time.Now()
	var infoErr error
	_, infoErr = s.Docker.Info(r.Context())
	metrics.ObserveDocker("info", startInfo, &infoErr)
	if infoErr != nil {
		writeErr(w, http.StatusServiceUnavailable, "docker info: "+infoErr.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
