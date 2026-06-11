// sandboxd — control plane for the sandboxd.
//
// CLAUDE.md control-plane scope: "Single Go binary, ~500–800 LOC
// target. Binds to 127.0.0.1 only. No auth in v1 (introduced in
// Phase 8). Shells out to `docker` CLI via os/exec."
//
// Phase 4 added: SQLite migrations + open, boot-time reconciler,
// HTTP API on 127.0.0.1, signal handling, graceful shutdown.
//
// Phase 5 adds: access-log tailer goroutine, optional Traefik
// open-connection poller goroutine (fallback if the metric can't be
// verified), the 30-second idle reaper, the 10-second host-memory
// pressure reaper, and the /wake/{id} handler (both Traefik
// catch-all and programmatic shapes).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sandboxd/control-plane/internal/activity"
	"github.com/sandboxd/control-plane/internal/api"
	"github.com/sandboxd/control-plane/internal/audit"
	"github.com/sandboxd/control-plane/internal/auth"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/idlock"
	"github.com/sandboxd/control-plane/internal/logging"
	"github.com/sandboxd/control-plane/internal/loopback"
	"github.com/sandboxd/control-plane/internal/metrics"
	nginxwatch "github.com/sandboxd/control-plane/internal/nginx"
	"github.com/sandboxd/control-plane/internal/reaper"
	"github.com/sandboxd/control-plane/internal/reconcile"
	"github.com/sandboxd/control-plane/internal/snapshot"
	"github.com/sandboxd/control-plane/internal/store"
	"github.com/sandboxd/control-plane/internal/wake"
)

const (
	// OSS default: sandboxd runs in its own container and Traefik
	// reaches it over the compose network by service name, so it binds
	// all interfaces (it is NOT published to the host — only reachable
	// on the internal sandboxd_net). Override with SANDBOXD_ADDR.
	defaultListenAddr = "0.0.0.0:9000"
	defaultImage      = "sandboxd-base:1.0.0"
	migrationsDir     = "/usr/local/share/sandboxd/migrations"

	// Default data root for the portable build. The compose file
	// bind-mounts this path host:container symmetric, so it is a valid
	// host path for the sibling `docker run -v`. Override with
	// SANDBOXD_DATA_DIR / SANDBOXD_LOG_DIR (paths derived in main()).
	defaultDataDir = "/var/lib/sandboxed"
	defaultLogDir  = "/var/log/sandboxed"

	// Idle / pressure / wake defaults. Each overridable via env; see
	// .env.example and README "Configuration".
	defaultIdleThresholdSec    = 2100 // 35 min idle → docker stop
	defaultIdleReapIntervalSec = 30
	defaultPressureIntervalSec = 10
	defaultMemHeadroomPct      = 15
	defaultMemRefusePct        = 10
	defaultMemEmergencyPct     = 5
	defaultWakeCostMB          = 800
	defaultWakeTCPReadySec     = 8
	defaultWakeGraceSec        = 60
	defaultKeepaliveMaxSec     = 86400

	// Snapshot subsystem defaults (auto-snapshotter is disabled in the
	// OSS build — see main(); these remain for the manual API surface).
	defaultSnapshotIntervalSec   = 3600
	defaultSnapshotRetentionDays = 7
	defaultSnapshotIdleHours     = 24
)

func main() {
	// Phase 8 — one-shot subcommands run and exit before the daemon
	// startup path. `sandboxd backfill-legacy ...` is the only one.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backfill-legacy":
			os.Exit(runBackfillLegacy(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "sandboxd: unknown subcommand %q\n", os.Args[1])
			os.Exit(2)
		}
	}

	log := logging.NewLogger()

	addr := envDefault("SANDBOXD_ADDR", defaultListenAddr)
	image := envDefault("SANDBOXD_IMAGE", defaultImage)
	// OSS default preview domain is "localhost": browsers resolve any
	// *.localhost name to 127.0.0.1, so preview URLs work out of the box
	// with no DNS and no certificates. Set PREVIEW_DOMAIN to a real
	// wildcard domain (plus PREVIEW_ENTRYPOINT=websecure, PREVIEW_TLS=true)
	// for a public deployment.
	domain := envDefault("PREVIEW_DOMAIN", "localhost")

	// Data + log roots. All per-sandbox workspace paths derive from
	// dataDir; the compose file bind-mounts dataDir host:container
	// symmetric so the path sandboxd writes is also a valid host path
	// for the sibling `docker run -v <workspace>:/home/sandbox`.
	dataDir := envDefault("SANDBOXD_DATA_DIR", defaultDataDir)
	logDir := envDefault("SANDBOXD_LOG_DIR", defaultLogDir)
	stateDir := filepath.Join(dataDir, "state")
	dbPath := filepath.Join(stateDir, "sandboxd.db")
	workspacesRoot := filepath.Join(dataDir, "workspaces")
	snapshotsRoot := filepath.Join(dataDir, "_snapshots")
	templatesRoot := filepath.Join(dataDir, "templates")
	libraryRoot := filepath.Join(dataDir, "library")
	accessLogPath := filepath.Join(logDir, "traefik-access.log")
	tailerOffsetFs := filepath.Join(stateDir, "traefik-tail.offset")

	// OSS docker-native toggles (default to the portable behaviour).
	network := os.Getenv("SANDBOXD_NETWORK")        // shared docker network for Traefik routing
	userns := envDefault("SANDBOXD_USERNS", "host") // sandbox + seed --userns; "host" is deterministic on any daemon
	previewEntrypoint := envDefault("PREVIEW_ENTRYPOINT", "web")
	previewTLS := boolFromEnv("PREVIEW_TLS", false)
	setMemoryHigh := boolFromEnv("SANDBOXD_SET_MEMORY_HIGH", false)

	migrations := envDefault("SANDBOXD_MIGRATIONS", migrationsDir)
	if _, err := os.Stat(migrations); err != nil {
		if exe, e := os.Executable(); e == nil {
			alt := filepath.Join(filepath.Dir(exe), "..", "..", "migrations")
			if _, e2 := os.Stat(alt); e2 == nil {
				migrations = alt
			}
		}
	}

	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		log.Error("startup: mkdir state dir failed", "err", err.Error())
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn := fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_fk=1", envDefault("SANDBOXD_DB", dbPath))
	st, err := store.Open(ctx, dsn, migrations)
	if err != nil {
		log.Error("startup: store open failed", "err", err.Error())
		os.Exit(1)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("shutdown: store close failed", "err", err.Error())
		}
	}()

	// Phase 5 — Backfill last_active_at for legacy running rows where
	// the migration default (0) would otherwise make every existing
	// row an idle candidate the moment the daemon comes up.
	if n, err := st.BackfillRunningActivity(ctx); err != nil {
		log.Warn("startup: backfill last_active_at failed", "err", err.Error())
	} else if n > 0 {
		log.Info("startup: backfilled last_active_at for running rows", "rows", n)
	}

	// Phase 8 — audit logger + service-token auth middleware. The
	// initial config is read from the process environment (systemd has
	// already loaded the EnvironmentFile); SIGHUP re-reads the file.
	auditLog := audit.New(st, log.With("component", "audit"))
	envFile := envDefault("SANDBOXD_ENV_FILE", "/etc/sandboxed/sandboxd.env")
	authMw := auth.NewMiddleware(auth.ParseConfig(os.Getenv), auditLog, log.With("component", "auth"))
	denyMode := envDefault("SANDBOXD_FORWARD_AUTH_DENY_MODE", "redirect")
	{
		ac := authMw.Snapshot()
		log.Info("startup: auth configured",
			"api_tokens", len(ac.APITokens),
			"preview_secrets", len(ac.PreviewSecrets),
			"auth_disabled", ac.Disabled,
			"forward_auth_deny_mode", denyMode,
		)
		if len(ac.APITokens) == 0 && !ac.Disabled {
			log.Warn("startup: SANDBOXD_API_TOKENS is empty — every external API call will 401 (loopback still works)")
		}
	}

	dockerClient := docker.NewClient()
	loopMgr := loopback.New()
	loopMgr.Root = workspacesRoot
	loopMgr.SeedImage = image
	loopMgr.DockerBin = "docker"
	loopMgr.Userns = userns

	// Egress (nftables) is DISABLED in the portable OSS build: it
	// requires host nftables + journald + systemd-timer refresh jobs
	// that don't exist in a plain docker-compose deployment. Every
	// consumer (reconciler, reapers, wake, API) treats a nil egress
	// manager as "no egress policy" — exactly the default-allow
	// behaviour, minus the connection logging. A nil manager here is
	// the single switch that keeps all of that off.
	var egressMgr *egress.Manager = nil

	// Phase 7 — shared per-id lock + snapshot subsystem. The snapshot
	// Manager is handed to the reconciler so its boot pass can sweep
	// crash debris (stale .tmp snapshots, interrupted-restore .bak).
	idLocks := idlock.New()
	snapshotMgr := &snapshot.Manager{
		WorkspacesRoot: workspacesRoot,
		SnapshotsRoot:  snapshotsRoot,
		RetentionDays:  intFromEnv("SANDBOXD_SNAPSHOT_RETENTION_DAYS", defaultSnapshotRetentionDays),
		IdleThreshold:  time.Duration(intFromEnv("SANDBOXD_SNAPSHOT_IDLE_HOURS", defaultSnapshotIdleHours)) * time.Hour,
		Store:          st,
		Locks:          idLocks,
		Log:            log.With("component", "snapshot"),
	}

	version, gitCommit := buildIdent()
	metrics.BuildInfo.WithLabelValues(version, gitCommit).Set(1)
	if err := metrics.RefreshSandboxGauge(ctx, st); err != nil {
		log.Warn("startup: refresh sandbox gauge failed", "err", err.Error())
	}

	// Reconciler once, before the listener.
	rcDeps := reconcile.Deps{
		Store:         st,
		Docker:        dockerClient,
		Loopback:      loopMgr,
		Egress:        egressMgr,
		Snapshot:      snapshotMgr,
		Log:           log.With("component", "reconcile"),
		SetMemoryHigh: setMemoryHigh,
	}
	res, err := reconcile.Once(ctx, rcDeps)
	metrics.ReconcilerRuns.Inc()
	metrics.ReconcilerLastDuration.Set(res.Duration.Seconds())
	for kind, n := range res.Orphans {
		metrics.ReconcilerOrphans.WithLabelValues(kind).Set(float64(n))
	}
	if err != nil {
		log.Error("startup: reconcile failed (continuing)", "err", err.Error())
	} else {
		log.Info("startup: reconcile complete",
			"rows", res.Rows,
			"reapplied", res.Reapplied,
			"stopped", res.Stopped,
			"errored", res.Errored,
			"egress_added", res.EgressAdded,
			"orphan_containers", res.Orphans["container"],
			"orphan_mounts", res.Orphans["mount"],
			"duration_ms", res.Duration.Milliseconds(),
		)
	}
	// Egress is disabled in the OSS build (egressMgr == nil); the
	// sources-active gauge is therefore always zero.
	metrics.EgressSourcesActive.Set(0)
	_ = metrics.RefreshSandboxGauge(ctx, st)

	// Phase 5 env knobs.
	idleThreshold := durationFromEnvSec("SANDBOXD_IDLE_THRESHOLD_SECONDS", defaultIdleThresholdSec)
	idleInterval := durationFromEnvSec("SANDBOXD_IDLE_REAP_INTERVAL_SECONDS", defaultIdleReapIntervalSec)
	pressureInterval := durationFromEnvSec("SANDBOXD_PRESSURE_INTERVAL_SECONDS", defaultPressureIntervalSec)
	headroomPct := floatFromEnv("SANDBOXD_MEM_HEADROOM_PCT", defaultMemHeadroomPct)
	refuseWakesPct := floatFromEnv("SANDBOXD_MEM_REFUSE_WAKES_PCT", defaultMemRefusePct)
	emergencyPct := floatFromEnv("SANDBOXD_MEM_EMERGENCY_PCT", defaultMemEmergencyPct)
	wakeCostMB := uintFromEnv("SANDBOXD_WAKE_COST_MB", defaultWakeCostMB)
	wakeTCPReady := durationFromEnvSec("SANDBOXD_WAKE_TCP_READY_TIMEOUT_SECONDS", defaultWakeTCPReadySec)
	wakeGrace := durationFromEnvSec("SANDBOXD_WAKE_GRACE_SECONDS", defaultWakeGraceSec)
	keepaliveMax := durationFromEnvSec("SANDBOXD_KEEPALIVE_MAX_SECONDS", defaultKeepaliveMaxSec)

	inflight := activity.NewInflightExec()
	refused := &atomic.Bool{}

	// Pressure reaper — long-lived. Also used synchronously by the
	// wake admission code, so we instantiate it before the wake
	// handler.
	pressure := &reaper.Pressure{
		Cfg: reaper.PressureConfig{
			Interval:       pressureInterval,
			HeadroomPct:    headroomPct,
			RefuseWakesPct: refuseWakesPct,
			EmergencyPct:   emergencyPct,
		},
		Store:    st,
		Docker:   dockerClient,
		Inflight: inflight,
		Egress:   egressMgr,
		Refused:  refused,
		Log:      log.With("component", "pressure-reaper"),
	}

	admitCfg := wake.AdmitConfig{
		WakeCostMB: wakeCostMB,
		FloorPct:   refuseWakesPct,
		Refused:    refused,
		Tick:       pressure.Tick,
	}

	wakeHandler, err := wake.New(
		st, dockerClient, domain,
		wake.Config{
			TCPReadyTimeout: wakeTCPReady,
			RefreshSeconds:  2,
		},
		admitCfg,
		egressMgr,
		idLocks,
		log.With("component", "wake"),
	)
	if err != nil {
		log.Error("startup: wake handler init failed", "err", err.Error())
		os.Exit(1)
	}
	// Phase 8 — gate stopped private-sandbox wakes through the same
	// preview-token check as /forward-auth.
	wakeHandler.Auth = authMw
	wakeHandler.Audit = auditLog
	wakeHandler.ForwardAuthDenyMode = denyMode
	wakeHandler.SetMemoryHigh = setMemoryHigh

	server := &api.Server{
		Store:               st,
		Docker:              dockerClient,
		Loopback:            loopMgr,
		Log:                 log.With("component", "api"),
		PreviewDomain:       domain,
		Image:               image,
		Network:             network,
		Userns:              userns,
		PreviewEntrypoint:   previewEntrypoint,
		PreviewTLS:          previewTLS,
		SetMemoryHigh:       setMemoryHigh,
		Inflight:            inflight,
		Wake:                wakeHandler,
		Admit:               admitCfg,
		KeepaliveMax:        keepaliveMax,
		Egress:              egressMgr,
		Snapshot:            snapshotMgr,
		Locks:               idLocks,
		Auth:                authMw,
		Audit:               auditLog,
		SnapshotsRoot:       snapshotsRoot,
		ForwardAuthDenyMode: denyMode,
		TemplatesDir:        envDefault("SANDBOXD_TEMPLATES_DIR", templatesRoot),
		LibraryRoot:         envDefault("SANDBOXD_LIBRARY_DIR", libraryRoot),
		LLMTxtPath:          envDefault("SANDBOXD_LLM_TXT_PATH", "/etc/sandboxed/llm.txt"),
		GitTokenPath:        envDefault("SANDBOXD_GIT_TOKEN_PATH", "/etc/sandboxed/git/token"),
	}

	// Finalize any coding task left `running` by a previous sandboxd
	// run before the idle reaper (which trusts the task table) starts.
	server.ReconcileTasks(ctx)

	// Phase 5 — roadmap §10: after reconcile, if MemAvailable is
	// already below the healthy floor, run one synchronous pressure
	// tick before opening the listener. Keeps the host from
	// accepting requests while it's already saturated.
	if mi, err := reaper.ReadMemInfo(); err == nil {
		metrics.MemAvailablePercent.Set(mi.AvailablePct())
		metrics.MemAvailableBytes.Set(float64(mi.AvailableBytes()))
		if mi.AvailablePct() < headroomPct {
			log.Warn("startup: MemAvailable below headroom — running pressure tick before listener opens",
				"avail_pct", mi.AvailablePct())
			pressure.Tick(ctx)
		}
	}

	// Mux + middleware: catch-all dispatch in front of the API mux so
	// the loopback-only listener handles both the loopback API and
	// the Traefik catch-all reverse-proxied traffic. The Host header
	// is the only discriminator.
	// Phase 8 — the service-token auth middleware wraps the API mux
	// only. The wake catch-all (dispatched first by hostDispatch) is
	// the browser preview path and stays unauthenticated; a private
	// sandbox's stopped-wake is gated inside the wake handler itself.
	apiMux := authMw.Wrap(server.Handler())
	root := hostDispatch(wakeHandler, apiMux, log)
	root = logging.Middleware(log, root)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// --- Phase 5 background goroutines ---------------------------------
	gctx, gcancel := context.WithCancel(context.Background())
	defer gcancel()

	// Idle reaper.
	idle := &reaper.Idle{
		Cfg: reaper.IdleConfig{
			Threshold: idleThreshold,
			Interval:  idleInterval,
			WakeGrace: wakeGrace,
		},
		Store:    st,
		Docker:   dockerClient,
		Inflight: inflight,
		Egress:   egressMgr,
		Log:      log.With("component", "idle-reaper"),
	}
	go func() {
		if err := idle.Run(gctx); err != nil {
			log.Warn("idle reaper exited", "err", err.Error())
		}
	}()

	// Pressure reaper.
	go func() {
		if err := pressure.Run(gctx); err != nil {
			log.Warn("pressure reaper exited", "err", err.Error())
		}
	}()

	// Access-log tailer.
	tailer := &activity.Tailer{
		LogPath:        envDefault("SANDBOXD_ACCESS_LOG", accessLogPath),
		CheckpointPath: envDefault("SANDBOXD_TAILER_OFFSET", tailerOffsetFs),
		PreviewDomain:  domain,
		Store:          st,
		Log:            log.With("component", "access-log-tailer"),
	}
	go func() {
		if err := tailer.Run(gctx); err != nil {
			log.Warn("access-log tailer exited", "err", err.Error())
		}
	}()

	// Open-connection poller — only if the operator has supplied a
	// verified metric name via env. Without the env, the poller logs
	// "fallback mode" and returns; the access-log tailer alone
	// drives last_active_at and the wider WakeGrace window
	// compensates for long-lived WS quiet periods.
	pollerMetric := os.Getenv("SANDBOXD_POLLER_METRIC_RE")
	pollerLabel := envDefault("SANDBOXD_POLLER_SERVICE_LABEL", "service")
	pollerURL := envDefault("SANDBOXD_POLLER_URL", "http://127.0.0.1:8082/metrics")
	pollerInterval := durationFromEnvSec("SANDBOXD_POLLER_INTERVAL_SECONDS", 15)
	var pollerRE *regexp.Regexp
	if pollerMetric != "" {
		var perr error
		pollerRE, perr = regexp.Compile(pollerMetric)
		if perr != nil {
			log.Warn("startup: SANDBOXD_POLLER_METRIC_RE invalid — running poller in fallback",
				"err", perr.Error())
			pollerRE = nil
		}
	}
	poller := &activity.Poller{
		MetricsURL:    pollerURL,
		Interval:      pollerInterval,
		MetricNameRE:  pollerRE,
		ServiceLabel:  pollerLabel,
		PreviewDomain: domain,
		Store:         st,
		Log:           log.With("component", "connection-poller"),
	}
	go func() {
		if err := poller.Run(gctx); err != nil {
			log.Warn("connection poller exited", "err", err.Error())
		}
	}()

	// --- Egress goroutines: DISABLED in the OSS build -----------------
	// Earlier deployments ran a journald-tail egress collector, an
	// nftables drop-counter poller, and a systemd refresh-job watcher.
	// All three depend on host nftables / journald / systemd, which a
	// portable docker-compose deployment does not provide. With
	// egressMgr == nil there is nothing to collect, so these goroutines
	// are intentionally not started.
	_ = egressMgr // documents the deliberate nil

	// --- nginx registry-proxy hot-reloader: DISABLED in the OSS build -
	// Earlier deployments ran a single host-side nginx caching proxy
	// for npm/pypi/crates/bun and hot-reloaded it on config change. The
	// OSS image points package managers at the public registries
	// directly, so there is no proxy container to watch. Re-enable by
	// running your own proxy and setting SANDBOXD_NGINX_WATCH_PATHS +
	// SANDBOXD_NGINX_CONTAINER (the watcher code is retained).
	_ = nginxwatch.ExecResult{} // keep the import referenced

	// --- Auto-snapshotter: DISABLED in the OSS build ------------------
	// The hourly auto-snapshotter zstd-compresses each sandbox's
	// workspace image. The portable build stores workspaces as plain
	// directories (not loopback .img files), so the compress path does
	// not apply. The manual snapshot/template REST endpoints remain
	// wired but are EXPERIMENTAL on directory storage (see README).
	_ = snapshotMgr // constructed for the reconciler debris sweep + API

	// --- Phase 8 workspace_owner orphan check -------------------------
	// roadmap §13 — every 6 h, log workspace_owner rows whose .img is
	// gone. Never deletes; disposition is the operator's.
	go func() {
		ownerLog := log.With("component", "owner-orphan-check")
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return
			case <-t.C:
				if n := reconcile.CheckWorkspaceOwnerOrphans(gctx, st, loopMgr, ownerLog); n > 0 {
					ownerLog.Warn("workspace_owner rows with no .img on disk", "count", n)
				}
			}
		}
	}()

	// Listen + serve.
	errCh := make(chan error, 1)
	go func() {
		log.Info("startup: listening",
			"addr", addr,
			"preview_domain", domain,
			"image", image,
			"idle_threshold", idleThreshold.String(),
			"idle_interval", idleInterval.String(),
			"pressure_interval", pressureInterval.String(),
			"mem_headroom_pct", headroomPct,
			"mem_refuse_wakes_pct", refuseWakesPct,
			"mem_emergency_pct", emergencyPct,
			"wake_cost_mb", wakeCostMB,
			"wake_tcp_ready", wakeTCPReady.String(),
			"wake_grace", wakeGrace.String(),
			"poller_mode", pollerModeLabel(pollerRE),
		)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Block until a terminating signal or server-error. SIGHUP is not
	// terminating — it re-reads the auth config (token rotation) and
	// the loop continues. SIGINT / SIGTERM break out to shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				log.Info("reload: SIGHUP — re-reading auth config", "env_file", envFile)
				if env, err := auth.LoadEnvFile(envFile); err != nil {
					log.Error("reload: read env file failed (keeping current config)",
						"err", err.Error())
				} else {
					nc := auth.ParseConfig(auth.MapGetter(env))
					authMw.Reload(nc)
					log.Info("reload: auth config reloaded",
						"api_tokens", len(nc.APITokens),
						"preview_secrets", len(nc.PreviewSecrets),
						"auth_disabled", nc.Disabled)
				}
				continue
			}
			log.Info("shutdown: signal received", "signal", sig.String())
		case err := <-errCh:
			log.Error("shutdown: server error", "err", err.Error())
		}
		break
	}

	gcancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown: http server shutdown failed", "err", err.Error())
	}
}

// hostDispatch routes incoming requests to either the wake catch-all
// (when Host header matches s-<id>-<port>.preview.<domain>) or the
// loopback API. Both share the same listener so the operator only
// has to wire one entry into Traefik's file provider.
func hostDispatch(w *wake.Handler, apiMux http.Handler, _ any) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w != nil && w.HostMatchesPreview(r.Host) {
			w.ServeCatchAll(rw, r)
			return
		}
		apiMux.ServeHTTP(rw, r)
	})
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envSplit returns the env var split by `sep`, trimming empty entries.
// Falls back to `def` (also split) if unset. Returns nil if the
// resulting list is empty (i.e. operator deliberately disabled).
func envSplit(k, def, sep string) []string {
	raw := os.Getenv(k)
	if raw == "" {
		raw = def
	}
	if raw == "" {
		return nil
	}
	parts := []string{}
	for _, p := range splitNonEmpty(raw, sep) {
		parts = append(parts, p)
	}
	return parts
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if string(r) == sep {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// dockerExecAdapter bridges *docker.Client to nginxwatch.Execer
// (same shape, different ExecResult type, avoiding an import cycle if
// docker grew nginx-specific types later).
type dockerExecAdapter struct{ d *docker.Client }

func (a dockerExecAdapter) Exec(ctx context.Context, name string, cmd []string) (nginxwatch.ExecResult, error) {
	r, err := a.d.Exec(ctx, name, cmd)
	return nginxwatch.ExecResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}, err
}

func durationFromEnvSec(k string, dSec int) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return time.Duration(dSec) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(dSec) * time.Second
	}
	if n < 0 {
		n = 0
	}
	return time.Duration(n) * time.Second
}

func floatFromEnv(k string, d float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return d
	}
	return f
}

func uintFromEnv(k string, d uint64) uint64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return d
	}
	return n
}

func intFromEnv(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}

// boolFromEnv parses a boolean env var. Accepts 1/true/yes/on (any
// case) as true and 0/false/no/off as false; anything else (including
// unset) returns the default.
func boolFromEnv(k string, d bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return d
	}
}

func pollerModeLabel(re *regexp.Regexp) string {
	if re == nil {
		return "fallback"
	}
	return "active"
}

func buildIdent() (version, gitCommit string) {
	version = "dev"
	gitCommit = "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				gitCommit = s.Value
				if len(gitCommit) > 12 {
					gitCommit = gitCommit[:12]
				}
			}
		}
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
	}
	return version, gitCommit
}
