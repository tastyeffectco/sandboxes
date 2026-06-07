package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/cgroup"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/idlock"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// Handler implements both HTML and JSON wake entry points.
//
//   - Traefik catch-all: arrives as any HTTP method/path with Host
//     header `s-<id>-<port>.preview.<domain>`. ServeHTTP detects the
//     shape via the Host regex and returns an HTML meta-refresh.
//
//   - Programmatic: arrives at POST /wake/{id} on the loopback API.
//     The path-value provides the id; Accept: application/json (or
//     no Host that matches preview shape) → JSON.
type Handler struct {
	Store         *store.Store
	Docker        *docker.Client
	PreviewDomain string
	Cfg           Config
	Admit         AdmitConfig
	Log           *slog.Logger

	// Phase 6 — egress sources policy. nil-safe (the wake path still
	// works without egress; the kernel just won't see the new bridge
	// IP, which is the same as Phase 5's pre-Phase-6 behaviour).
	Egress *egress.Manager

	// Phase 7 — shared per-id lock so snapshot/restore/destroy exclude
	// an in-progress wake. nil-safe.
	Locks *idlock.Registry

	// Phase 8 — private-sandbox wake gating. A stopped private sandbox
	// must not be woken anonymously: the catch-all wake runs the same
	// preview-token check as /forward-auth before docker start
	// (roadmap §10). All nil-safe — without Auth wired the wake path
	// behaves exactly as in Phase 7 (no gate).
	Auth                *auth.Middleware
	Audit               *audit.Logger
	ForwardAuthDenyMode string // "redirect" (default) | "meta-refresh"

	// SetMemoryHigh gates the cgroup-v2 memory.high re-apply after a
	// wake. Off in the portable OSS build (no host cgroup access); the
	// --memory ceiling on the container still applies.
	SetMemoryHigh bool

	hostRE *regexp.Regexp

	mu        sync.Mutex
	inflight  map[string]*inflightWake
}

// Config holds the env-tunable knobs the handler needs.
type Config struct {
	TCPReadyTimeout time.Duration // SANDBOXD_WAKE_TCP_READY_TIMEOUT_SECONDS (8s)
	RefreshSeconds  int           // meta-refresh content value (2)
}

type inflightWake struct {
	done chan struct{}
	err  error
}

// jsonWakeResp is the success body for the JSON path.
type jsonWakeResp struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	WakeDurationMS int64  `json:"wake_duration_ms"`
}

// jsonErrResp is the failure body for the JSON path.
type jsonErrResp struct {
	Error             string  `json:"error"`
	MemAvailablePercent float64 `json:"mem_available_percent,omitempty"`
}

// New returns a Handler. The host regex is compiled once.
func New(s *store.Store, d *docker.Client, previewDomain string, cfg Config, admit AdmitConfig, eg *egress.Manager, locks *idlock.Registry, log *slog.Logger) (*Handler, error) {
	if cfg.RefreshSeconds <= 0 {
		cfg.RefreshSeconds = 2
	}
	if cfg.TCPReadyTimeout <= 0 {
		cfg.TCPReadyTimeout = 8 * time.Second
	}
	pat := `^s-([0-9A-Za-z]+)-([0-9]+)\.preview\.` +
		regexp.QuoteMeta(previewDomain) + `(?::\d+)?$`
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("compile preview-host regex: %w", err)
	}
	return &Handler{
		Store:         s,
		Docker:        d,
		PreviewDomain: previewDomain,
		Cfg:           cfg,
		Admit:         admit,
		Egress:        eg,
		Locks:         locks,
		Log:           log,
		hostRE:        re,
		inflight:      map[string]*inflightWake{},
	}, nil
}

// ServeCatchAll handles the Traefik catch-all request. Host header
// MUST already match the preview shape — main.go's middleware does
// the gate; this handler does not double-check (returns 400 if the
// shape doesn't match anyway).
func (h *Handler) ServeCatchAll(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	m := h.hostRE.FindStringSubmatch(host)
	if m == nil {
		http.Error(w, "host header does not match preview shape", http.StatusBadRequest)
		return
	}
	// Browsers and proxies lowercase the Host header, but sandbox ids
	// are canonical uppercase ULIDs and the DB lookup is case-
	// sensitive — normalise, or every browser-initiated preview wake
	// 404s as not_found while the task path (exact-case id) works.
	id := strings.ToUpper(m[1])
	port := m[2]
	h.serve(r, w, id, port, true)
}

// ServeJSON handles the programmatic POST /wake/{id} entry point.
func (h *Handler) ServeJSON(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, jsonErrResp{Error: "missing id"})
		return
	}
	h.serve(r, w, id, "", false)
}

// serve is the shared body. isHTML selects the response shape; the
// request is needed for the Phase 8 private-sandbox cookie check.
func (h *Handler) serve(r *http.Request, w http.ResponseWriter, id, port string, isHTML bool) {
	ctx := r.Context()
	log := h.Log.With("sandbox_id", id, "shape", shapeOf(isHTML))
	start := time.Now()

	// Per-id mutex to dedup concurrent wakes. Roadmap §7 idempotency
	// rule: "two concurrent wake requests must not double-start.
	// Guard per-id with an in-memory mutex; the second caller waits
	// and reuses the first caller's outcome."
	h.mu.Lock()
	if wf, ok := h.inflight[id]; ok {
		h.mu.Unlock()
		<-wf.done
		if wf.err != nil {
			log.Info("wake: rode previous inflight to failure", "err", wf.err.Error())
			h.respondError(w, id, "concurrent_wake_failed", isHTML)
			metrics.Wakes.WithLabelValues("error").Inc()
			return
		}
		h.respondSuccess(w, id, time.Since(start), isHTML)
		metrics.Wakes.WithLabelValues("success").Inc()
		return
	}
	wf := &inflightWake{done: make(chan struct{})}
	h.inflight[id] = wf
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.inflight, id)
		h.mu.Unlock()
		close(wf.done)
	}()

	// Phase 7 — hold the shared per-id lock for the whole wake. The
	// inflight map above already dedups concurrent *wakes*; this lock
	// additionally excludes a concurrent snapshot / restore / destroy
	// of the same id (roadmap phase-7 §9: snapshot must not race a
	// wake that is starting the container and writing the loopback).
	// nil-safe — pre-Phase-7 callers that build a Handler without a
	// lock registry still work.
	if h.Locks != nil {
		h.Locks.Lock(id)
		defer h.Locks.Unlock(id)
	}

	// 1. Look up the row.
	sb, err := h.Store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Info("wake: no sandbox row for that id")
			h.respondNotFound(w, id, isHTML)
			metrics.Wakes.WithLabelValues("not_found").Inc()
			return
		}
		wf.err = err
		log.Warn("wake: store.Get failed", "err", err.Error())
		h.respondError(w, id, "internal_error", isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	}

	switch sb.Status {
	case "running":
		// Traefik may not have observed the route yet (catch-all
		// path), or the user just hit /wake on a live sandbox
		// (programmatic path). Either way, return success → the
		// browser refreshes, the live route wins.
		log.Info("wake: already running")
		h.respondSuccess(w, id, time.Since(start), isHTML)
		metrics.Wakes.WithLabelValues("success").Inc()
		return
	case "error":
		msg := "error_status"
		if sb.ErrorMessage.Valid {
			msg = sb.ErrorMessage.String
		}
		log.Warn("wake: row in error state", "error_message", msg)
		h.respondError(w, id, msg, isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	case "creating":
		// Half-built row. Tell the caller to retry; don't start a
		// container under it.
		log.Info("wake: row is still creating; refusing to wake")
		h.respondError(w, id, "creating", isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	}
	// status == 'stopped' (only remaining valid case).

	// Phase 8 — a private sandbox must not be woken anonymously. Run
	// the same preview-token check as /forward-auth against the
	// request cookie BEFORE admission + docker start (roadmap §10).
	// Only the browser (catch-all) path is gated — the programmatic
	// JSON wake is a loopback/upstream call the upstream already
	// authorized.
	if isHTML && sb.Visibility == "private" {
		if denied := h.gatePrivateWake(w, r, sb); denied {
			log.Info("wake: private sandbox — preview auth required, redirected")
			metrics.Wakes.WithLabelValues("auth_denied").Inc()
			return
		}
	}

	// 2. Admission check.
	outcome, err := Admit(ctx, h.Admit)
	if err != nil {
		wf.err = err
		log.Warn("wake: admission read failed", "err", err.Error())
		h.respondError(w, id, "admission_read_failed", isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	}
	if !outcome.Admit {
		log.Info("wake: admission denied",
			"reason", outcome.Reason,
			"avail_pct", outcome.AvailPct,
		)
		h.respondAdmissionDenied(w, id, outcome, isHTML)
		metrics.Wakes.WithLabelValues("admission_denied").Inc()
		return
	}

	// 3. docker start (idempotent).
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := h.Docker.Start(startCtx, "s-"+id); err != nil {
		wf.err = err
		log.Warn("wake: docker start failed", "err", err.Error())
		h.respondError(w, id, "start_failed", isHTML)
		metrics.Wakes.WithLabelValues("start_failed").Inc()
		return
	}

	// 4. Inspect and re-apply memory.high.
	cj, err := h.Docker.Inspect(ctx, "s-"+id)
	if err != nil {
		wf.err = err
		log.Warn("wake: inspect after start failed", "err", err.Error())
		h.respondError(w, id, "inspect_failed", isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	}

	// 4b. Phase 6 — register the (possibly new) bridge IP with the
	// egress nftables set. Docker can reassign bridge IPs across a
	// daemon restart so always derive from the live inspect, not
	// from the stored row. Order: kernel set first, then DB. roadmap
	// §4 hard rule — abort the row if nft fails rather than running
	// without an egress policy attached.
	if h.Egress != nil {
		newIP := cj.BridgeIP()
		// Tear down any previously-recorded IP so we don't leave an
		// orphaned element pointing at a recycled docker0 address.
		if sb.ContainerIP.Valid && sb.ContainerIP.String != "" && sb.ContainerIP.String != newIP {
			_ = h.Egress.Remove(ctx, id, sb.ContainerIP.String)
		}
		if newIP == "" {
			wf.err = errors.New("no bridge IP after start")
			log.Warn("wake: no bridge IP — refusing to proceed without egress policy")
			h.respondError(w, id, "no_bridge_ip", isHTML)
			metrics.Wakes.WithLabelValues("error").Inc()
			return
		}
		if err := h.Egress.Add(ctx, id, newIP); err != nil {
			wf.err = err
			log.Warn("wake: egress.Add failed", "err", err.Error())
			h.respondError(w, id, "egress_failed", isHTML)
			metrics.Wakes.WithLabelValues("error").Inc()
			return
		}
		if err := h.Store.SetContainerIP(ctx, id, newIP); err != nil {
			log.Warn("wake: SetContainerIP failed (continuing; in-memory map already updated)",
				"err", err.Error())
		}
	}


	rel := sb.CgroupPath.String
	if h.SetMemoryHigh {
		memHigh := sb.MemoryHigh
		if memHigh == "" {
			memHigh = "4G"
		}
		if r2, err := cgroup.SetMemoryHigh(ctx, cj.State.Pid, memHigh); err != nil {
			// memory.high re-apply failure is logged but doesn't fail
			// the wake — the sandbox is up and serving; the --memory
			// ceiling still bounds it.
			log.Warn("wake: re-apply memory.high failed (continuing)", "err", err.Error())
		} else {
			rel = r2
		}
	}

	// 5. TCP-ready probe. The catch-all path knows the port; the
	// programmatic path doesn't and skips the probe (the caller is
	// responsible for any readiness check). For the catch-all, we
	// fall through to the meta-refresh page on timeout; the user
	// sees the refresh, the next request retries.
	if port != "" {
		ip := cj.BridgeIP()
		if ip == "" {
			log.Warn("wake: no bridge IP after start (Traefik discovery may absorb)")
		} else if !waitTCP(ctx, ip, port, h.Cfg.TCPReadyTimeout) {
			log.Warn("wake: TCP-ready probe timed out (falling through to refresh)",
				"ip", ip, "port", port, "timeout", h.Cfg.TCPReadyTimeout.String())
			metrics.Wakes.WithLabelValues("tcp_ready_timeout").Inc()
			// Still record on row + still serve refresh — the timeout
			// is informational, not a hard failure.
		}
	}

	// 6. Update row to running + bump last_active_at.
	now := time.Now().UTC()
	if err := h.Store.MarkRunningWoke(ctx, id, cj.ID, rel, now); err != nil {
		wf.err = err
		log.Warn("wake: MarkRunningWoke failed", "err", err.Error())
		h.respondError(w, id, "store_failed", isHTML)
		metrics.Wakes.WithLabelValues("error").Inc()
		return
	}

	dur := time.Since(start)
	metrics.WakeDuration.Observe(dur.Seconds())
	metrics.Wakes.WithLabelValues("success").Inc()
	log.Info("wake: success", "duration_ms", dur.Milliseconds())
	if h.Audit != nil {
		extUID := ""
		if sb.ExternalUserID.Valid {
			extUID = sb.ExternalUserID.String
		}
		h.Audit.Write(ctx, audit.Entry{
			ActorKind:      "system",
			ExternalUserID: extUID,
			Action:         "sandbox.wake",
			Target:         id,
			Detail:         map[string]any{"shape": shapeOf(isHTML), "duration_ms": dur.Milliseconds()},
		})
	}
	h.respondSuccess(w, id, dur, isHTML)
}

// gatePrivateWake runs the /forward-auth preview-token check against
// the request cookie for a stopped private sandbox. It returns true
// (and has already written a 302 / 401 response) when the wake must
// be refused; false when the caller may proceed. Fails OPEN when Auth
// is not wired — pre-Phase-8 behaviour.
func (h *Handler) gatePrivateWake(w http.ResponseWriter, r *http.Request, sb *store.Sandbox) bool {
	if h.Auth == nil {
		return false
	}
	// A service- or operator-authenticated caller (the upstream
	// backend or an operator) is already authorized — the
	// preview-cookie gate is only for end-users reaching the preview
	// URL. This is what lets wake-on-task-submit work for private
	// sandboxes; the end-user wake path (actor "unknown") is unchanged.
	if k := auth.ActorFrom(r.Context()).Kind; k == "service" || k == "operator" {
		return false
	}
	cfg := h.Auth.Snapshot()
	ownerUID := ""
	if wo, err := h.Store.GetWorkspaceOwner(r.Context(), sb.ID); err == nil {
		ownerUID = wo.ExternalUserID
	}
	cookieVal := ""
	if c, err := r.Cookie("sandbox_preview"); err == nil {
		cookieVal = c.Value
	}
	_, reason := auth.CheckPreviewAccess(cookieVal, sb.ID, ownerUID, cfg.PreviewSecrets, time.Now())
	if reason == "" {
		return false // allowed — proceed with the wake
	}
	if h.Audit != nil {
		h.Audit.Write(r.Context(), audit.Entry{
			ActorKind:      "system",
			ExternalUserID: ownerUID,
			Action:         "preview.access_denied",
			Target:         sb.ID,
			Detail:         map[string]any{"reason": reason, "via": "wake"},
		})
	}
	if cfg.AuthRedirectURL == "" {
		writeErrorPage(w, sb.ID, "private_auth_required")
		return true
	}
	returnURL := "https://" + r.Host + r.URL.RequestURI()
	target := auth.BuildRedirectURL(cfg.AuthRedirectURL, sb.ID, returnURL)
	if h.ForwardAuthDenyMode == "meta-refresh" {
		w.Header().Set("Location", target)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8">`+
			`<meta http-equiv="refresh" content="0;url=`+target+`"><title>Redirecting…</title>`+
			`</head><body><p>Redirecting you to sign in…</p></body></html>`+"\n")
		return true
	}
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

// --- response helpers ------------------------------------------------

func (h *Handler) respondSuccess(w http.ResponseWriter, id string, dur time.Duration, isHTML bool) {
	if isHTML {
		writeRefreshPage(w, http.StatusOK, id, h.Cfg.RefreshSeconds)
		return
	}
	writeJSON(w, http.StatusOK, jsonWakeResp{
		ID:             id,
		Status:         "running",
		WakeDurationMS: dur.Milliseconds(),
	})
}

func (h *Handler) respondNotFound(w http.ResponseWriter, id string, isHTML bool) {
	if isHTML {
		writeErrorPage(w, id, "not_found")
		return
	}
	writeJSON(w, http.StatusNotFound, jsonErrResp{Error: "not_found"})
}

func (h *Handler) respondError(w http.ResponseWriter, id, reason string, isHTML bool) {
	if isHTML {
		writeErrorPage(w, id, reason)
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, jsonErrResp{Error: reason})
}

func (h *Handler) respondAdmissionDenied(w http.ResponseWriter, id string, o Outcome, isHTML bool) {
	if isHTML {
		writeBusyPage(w, id, o.Reason, 30, o.AvailPct)
		return
	}
	w.Header().Set("Retry-After", "30")
	writeJSON(w, http.StatusServiceUnavailable, jsonErrResp{
		Error:              o.Reason,
		MemAvailablePercent: o.AvailPct,
	})
}

// --- helpers ---------------------------------------------------------

func waitTCP(ctx context.Context, ip, port string, total time.Duration) bool {
	deadline := time.Now().Add(total)
	addr := net.JoinHostPort(ip, port)
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		var d net.Dialer
		conn, err := d.DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func shapeOf(isHTML bool) string {
	if isHTML {
		return "html"
	}
	return "json"
}

// HostMatchesPreview is a helper exported so main.go can decide
// whether an incoming request should be dispatched to ServeCatchAll
// rather than the API mux. Centralised here so the regex shape lives
// in exactly one place.
func (h *Handler) HostMatchesPreview(host string) bool {
	if h.hostRE == nil {
		return false
	}
	// Trim any trailing :port — http.Request.Host can include it.
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		// Allow the regex's own (?::\d+)? to handle the suffix; only
		// strip when host contains a colon AND the regex doesn't
		// already match.
		if h.hostRE.MatchString(host) {
			return true
		}
		return h.hostRE.MatchString(host[:i])
	}
	return h.hostRE.MatchString(host)
}
