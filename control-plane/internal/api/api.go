// Package api wires the HTTP handlers. CLAUDE.md control-plane scope:
// "Binds to 127.0.0.1 only. No auth in v1 (introduced in Phase 8)."
// The phase-4 listener default is 127.0.0.1:8080.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sandboxd/control-plane/internal/activity"
	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/idlock"
	"github.com/sandboxd/control-plane/internal/loopback"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/snapshot"
	"github.com/sandboxd/control-plane/internal/store"
	"github.com/sandboxd/control-plane/internal/wake"
)

// Server bundles the collaborators the handlers need.
type Server struct {
	Store         *store.Store
	Docker        *docker.Client
	Loopback      *loopback.Manager
	Log           *slog.Logger
	PreviewDomain string
	Image         string

	// OSS docker-native knobs (see cmd/sandboxd/main.go for env wiring):
	//   Network           — shared docker network sandboxes join so Traefik
	//                        can route to them (e.g. sandboxd_net).
	//   PreviewEntrypoint — Traefik entrypoint on preview routers ("web"
	//                        plain HTTP by default, "websecure" for TLS).
	//   PreviewTLS        — emit tls=true on preview routers (needs a cert in
	//                        Traefik's default store; off by default).
	//   SetMemoryHigh     — write cgroup v2 memory.high after start. Needs
	//                        host cgroup access; off by default in the
	//                        portable build (the --memory ceiling still applies).
	Network           string
	Userns            string
	Runtime           string
	DNSResolvConf     string
	PreviewEntrypoint string
	PreviewTLS        bool
	SetMemoryHigh     bool

	// AgentCfg is the in-memory cache of the platform's agent
	// configuration (model + AGENTS.md) — the source of truth for
	// what each sandbox's coding agent uses. Reads are lock-free
	// (atomic.Pointer); writes via PUT /v1/agent-config bump a
	// monotonic Version so the per-task rewrite path refreshes each
	// sandbox's on-workspace files exactly once after a change.

	// TemplatesDir is the host directory holding prebuilt golden
	// template .img files for the fast-cold-start path
	// (ops/design/fast-coldstart-react-vite-snapshot.md). Empty
	// disables the optional `template` field on POST /sandbox.
	TemplatesDir string

	// LibraryRoot is the host directory holding user-created snapshot
	// images (ops/design/snapshots-as-templates.md) —
	// /var/lib/sandboxed/library/<snapshot_id>.img. Independent of
	// _snapshots/ (the Phase 7 auto-snapshot tree) so the retention
	// pruner and per-sandbox purge never touch templates. Empty disables
	// the /v1/snapshots endpoints (503).
	LibraryRoot string

	// LLMTxtPath is the host file served at the public, tokenless
	// GET /llm.txt (the API contract for integrators). Empty → 404.
	LLMTxtPath string

	// GitTokenPath is the host file holding the master git push token
	// (auto-git-push). Read at push time; injected inline; never enters
	// a sandbox. Empty disables auto-git-push platform-wide.
	GitTokenPath string

	// Phase 5 additions — nil-safe so existing tests that build a
	// Server without these still work.
	Inflight     *activity.InflightExec
	Wake         *wake.Handler
	Admit        wake.AdmitConfig
	KeepaliveMax time.Duration

	// Phase 6 — egress policy hook. nil-safe.
	Egress *egress.Manager

	// Phase 7 — snapshot/restore subsystem + the shared per-id lock.
	// nil-safe: the snapshot endpoints return 503 if Snapshot is nil.
	Snapshot *snapshot.Manager
	Locks    *idlock.Registry

	// Phase 8 — service-token auth, audit log, preview-token gate.
	// All nil-safe: a Server built without these behaves like the
	// Phase 7 server (the auth middleware is applied separately in
	// main.go around this mux).
	Auth                *auth.Middleware
	Audit               *audit.Logger
	SnapshotsRoot       string // per-sandbox purge of _snapshots/<id>/
	ForwardAuthDenyMode string // "redirect" (default) | "meta-refresh"
}

// Handler returns the http.Handler ready for ListenAndServe.
// Wraps every route in the metric-recording middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /sandbox", s.observe("POST /sandbox", s.handleCreate))
	mux.HandleFunc("GET /sandboxes", s.observe("GET /sandboxes", s.handleList))
	mux.HandleFunc("GET /sandbox/{id}", s.observe("GET /sandbox/{id}", s.handleGet))
	mux.HandleFunc("DELETE /sandbox/{id}", s.observe("DELETE /sandbox/{id}", s.handleDelete))
	mux.HandleFunc("POST /sandbox/{id}/exec", s.observe("POST /sandbox/{id}/exec", s.handleExec))
	mux.HandleFunc("POST /sandbox/{id}/keepalive", s.observe("POST /sandbox/{id}/keepalive", s.handleKeepalive))
	mux.HandleFunc("POST /wake/{id}", s.observe("POST /wake/{id}", s.handleWakeJSON))
	mux.HandleFunc("POST /sandbox/{id}/snapshots", s.observe("POST /sandbox/{id}/snapshots", s.handleSnapshotTake))
	mux.HandleFunc("GET /sandbox/{id}/snapshots", s.observe("GET /sandbox/{id}/snapshots", s.handleSnapshotList))
	mux.HandleFunc("POST /sandbox/{id}/restore", s.observe("POST /sandbox/{id}/restore", s.handleSnapshotRestore))
	mux.HandleFunc("GET /llm.txt", s.observe("GET /llm.txt", s.handleLLMTxt))
	mux.HandleFunc("GET /healthz", s.observe("GET /healthz", s.handleHealthz))
	mux.HandleFunc("GET /readyz", s.observe("GET /readyz", s.handleReadyz))
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	// Phase 8 — external identity, purge, and preview-auth endpoints.
	mux.HandleFunc("POST /sandbox/{id}/claim", s.observe("POST /sandbox/{id}/claim", s.handleClaim))
	mux.HandleFunc("POST /sandbox/{id}/purge", s.observe("POST /sandbox/{id}/purge", s.handlePurgeSandbox))
	mux.HandleFunc("POST /external-users/{external_user_id}/purge", s.observe("POST /external-users/{id}/purge", s.handlePurgeExternalUser))
	mux.HandleFunc("POST /external-projects/{external_project_id}/purge", s.observe("POST /external-projects/{id}/purge", s.handlePurgeExternalProject))
	mux.HandleFunc("GET /preview-auth", s.observe("GET /preview-auth", s.handlePreviewAuth))
	mux.HandleFunc("GET /forward-auth", s.observe("GET /forward-auth", s.handleForwardAuth))

	// Public v1 API (ops/design/v1-external-api.md) — a narrow
	// translation layer over the internal machinery + runtimed.
	mux.HandleFunc("POST /v1/sandboxes", s.observe("POST /v1/sandboxes", s.v1CreateSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.observe("GET /v1/sandboxes/{id}", s.v1GetSandbox))
	mux.HandleFunc("POST /v1/sandboxes/{id}/stop", s.observe("POST /v1/sandboxes/{id}/stop", s.v1StopSandbox))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.observe("DELETE /v1/sandboxes/{id}", s.v1DeleteSandbox))
	mux.HandleFunc("POST /v1/sandboxes/{id}/tasks", s.observe("POST /v1/sandboxes/{id}/tasks", s.v1SubmitTask))
	mux.HandleFunc("GET /v1/sandboxes/{id}/tasks/{taskId}", s.observe("GET /v1/sandboxes/{id}/tasks/{taskId}", s.v1GetTask))
	mux.HandleFunc("GET /v1/sandboxes/{id}/tasks/{taskId}/events", s.observe("GET /v1/sandboxes/{id}/tasks/{taskId}/events", s.v1TaskEvents))
	mux.HandleFunc("POST /v1/sandboxes/{id}/tasks/{taskId}/cancel", s.observe("POST /v1/sandboxes/{id}/tasks/{taskId}/cancel", s.v1CancelTask))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.observe("GET /v1/sandboxes/{id}/files", s.v1ListFiles))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/content", s.observe("GET /v1/sandboxes/{id}/files/content", s.v1FileContent))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.observe("PUT /v1/sandboxes/{id}/files", s.v1PutFile))
	mux.HandleFunc("GET /v1/sandboxes/{id}/export", s.observe("GET /v1/sandboxes/{id}/export", s.v1Export))

	// Snapshots-as-templates (ops/design/snapshots-as-templates.md).
	mux.HandleFunc("POST /v1/snapshots", s.observe("POST /v1/snapshots", s.v1CreateSnapshot))
	mux.HandleFunc("GET /v1/snapshots", s.observe("GET /v1/snapshots", s.v1ListSnapshots))
	mux.HandleFunc("GET /v1/snapshots/{id}", s.observe("GET /v1/snapshots/{id}", s.v1GetSnapshot))
	mux.HandleFunc("DELETE /v1/snapshots/{id}", s.observe("DELETE /v1/snapshots/{id}", s.v1DeleteSnapshot))

	return mux
}

// observe records request count + duration per endpoint+method.
// (Logging middleware in the logging package wraps this from main.go.)
func (s *Server) observe(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h(sw, r)
		metrics.APIDuration.WithLabelValues(endpoint, r.Method).Observe(time.Since(start).Seconds())
		metrics.APIRequests.WithLabelValues(endpoint, r.Method, statusBucket(sw.status)).Inc()
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer's flusher. Without it this
// wrapper hides http.Flusher from streaming handlers (SSE task
// events), so Go buffers the whole response and the client receives
// every event in one burst at the end. See internal/api/v1_tasks.go.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func statusBucket(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
