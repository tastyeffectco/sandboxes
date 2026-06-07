// Package metrics owns the Prometheus collectors for sandboxd. The
// exposed set is exactly what the Phase 4 roadmap §12 lists; Phase 5
// will add reaper / wake-path metrics, Phase 7 wires the scrape
// destination.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sandboxd/control-plane/internal/store"
)

// Registry is the package-level Prometheus registry.
var Registry = prometheus.NewRegistry()

var (
	BuildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_build_info",
		Help: "Static build identifiers exposed as a gauge with value 1.",
	}, []string{"version", "git_commit"})

	SandboxesByStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_sandboxes_total",
		Help: "Number of sandboxes by status.",
	}, []string{"status"})

	APIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_api_requests_total",
		Help: "Count of API requests by endpoint, method, response code.",
	}, []string{"endpoint", "method", "code"})

	APIDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sandboxd_api_request_duration_seconds",
		Help:    "API request duration in seconds.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 7), // 1 ms .. 4 s
	}, []string{"endpoint", "method"})

	DockerDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sandboxd_docker_command_duration_seconds",
		Help:    "Wall time of docker CLI invocations by op.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 7), // 10 ms .. 40 s
	}, []string{"op"})

	DockerErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_docker_command_errors_total",
		Help: "Count of docker CLI invocations that returned an error.",
	}, []string{"op"})

	ReconcilerRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sandboxd_reconciler_runs_total",
		Help: "Total reconciler invocations since process start.",
	})

	ReconcilerLastDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_reconciler_last_duration_seconds",
		Help: "Duration of the most recent reconciler run.",
	})

	ReconcilerOrphans = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_reconciler_orphans_total",
		Help: "Orphan resources observed on the last reconciler run.",
	}, []string{"kind"})

	// ----- Phase 5 — idle + pressure + wake + activity -----

	IdleReaperRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sandboxd_idle_reaper_runs_total",
		Help: "Total idle-reaper ticks since process start.",
	})

	IdleReaperStops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_idle_reaper_stops_total",
		Help: "Sandboxes the idle reaper stopped, by reason.",
	}, []string{"reason"})

	PressureReaperRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sandboxd_pressure_reaper_runs_total",
		Help: "Total pressure-reaper ticks since process start.",
	})

	PressureReaperStops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_pressure_reaper_stops_total",
		Help: "Sandboxes the pressure reaper stopped, by band.",
	}, []string{"band"})

	MemAvailablePercent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_mem_available_percent",
		Help: "Host MemAvailable as a percentage of MemTotal (0..100).",
	})

	MemAvailableBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_mem_available_bytes",
		Help: "Host MemAvailable in bytes.",
	})

	Wakes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_wakes_total",
		Help: "Wake attempts by outcome.",
	}, []string{"outcome"}) // success | admission_denied | start_failed | tcp_ready_timeout | not_found | error

	WakeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sandboxd_wake_duration_seconds",
		Help:    "Wall time of a successful wake from request start to TCP-ready.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 8), // 100ms .. ~25s
	})

	WakesRefusedActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_wakes_refused_active",
		Help: "1 when the pressure reaper is currently refusing new wakes, else 0.",
	})

	InflightExec = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_inflight_exec_total",
		Help: "Number of in-flight POST /sandbox/{id}/exec calls across all sandboxes.",
	})

	AccessLogLagSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_access_log_lag_seconds",
		Help: "Seconds between now and the newest access-log line successfully parsed by the tailer.",
	})

	// ----- Phase 6 — egress collector + nft drops + refresh jobs -----

	EgressConnections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_egress_connections_total",
		Help: "Outbound TCP connections initiated by a sandbox, joined to sandbox_id.",
	}, []string{"sandbox_id", "dst_port_bucket"})

	EgressDrops = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_egress_drops_total",
		Help: "Packets dropped by the sandbox_platform nftables rules, by reason.",
	}, []string{"reason"}) // abuse | ssh | rfc1918 | metadata | smtp | cross_sandbox

	GitHostsRefreshRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_git_hosts_refresh_runs_total",
		Help: "Outcomes of the sandbox-git-hosts-refresh.service run.",
	}, []string{"outcome"}) // ok | failed

	AbuseListRefreshRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_abuse_list_refresh_runs_total",
		Help: "Outcomes of the sandbox-abuse-list-refresh.service run.",
	}, []string{"outcome"}) // ok | failed

	EgressLogLagSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_egress_log_lag_seconds",
		Help: "Seconds between now and the newest kernel egress log line processed by the collector.",
	})

	EgressSourcesActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_egress_sources_active",
		Help: "Number of IPs currently in the in-memory sandbox_sources map.",
	})

	// ----- Phase 7 — snapshot / restore subsystem -----

	SnapshotsTaken = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_snapshots_taken_total",
		Help: "Workspace snapshots attempted, by outcome.",
	}, []string{"outcome"}) // ok | error

	SnapshotRestores = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_snapshot_restores_total",
		Help: "Workspace restores attempted, by outcome.",
	}, []string{"outcome"}) // ok | error

	SnapshotterRuns = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sandboxd_snapshotter_runs_total",
		Help: "Total auto-snapshotter ticks since process start.",
	})

	SnapshotLastDurationSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_snapshot_last_duration_seconds",
		Help: "Wall time of the most recent successful snapshot.",
	})

	SnapshotLastSizeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sandboxd_snapshot_last_size_bytes",
		Help: "Compressed size of the most recent successful snapshot.",
	})

	// ----- Phase 9 — forward-auth instrumentation (capacity tests) ----

	// ForwardAuthDuration times every GET /forward-auth invocation —
	// the Traefik forward-auth hot path on private sandboxes. roadmap
	// phase-9 §8 targets p95 < 50 ms; the 0.05 bucket boundary makes
	// that quantile directly readable.
	ForwardAuthDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sandboxd_forward_auth_duration_seconds",
		Help:    "Wall time of a GET /forward-auth invocation (Traefik forward-auth hot path).",
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5},
	})

	// PreviewAccess counts forward-auth access decisions. roadmap
	// phase-9 §8 references `sandboxd_preview_access_*` for the
	// WebSocket-concurrency call-rate measurement.
	PreviewAccess = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_preview_access_total",
		Help: "Forward-auth access decisions, by result.",
	}, []string{"result"}) // allowed | denied

	// NginxReloads counts nginx safe-reload outcomes from the
	// internal/nginx file watcher. Operators alert on the failure
	// labels — they mean the registry-proxy is serving stale config
	// because the on-disk file is invalid.
	NginxReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_nginx_reloads_total",
		Help: "nginx safe-reload outcomes from the operator-managed config watcher.",
	}, []string{"outcome"}) // ok | validate_failed | reload_failed | exec_error | reload_exec_error

	// GitPush counts auto-git-push attempts on task finish, by outcome.
	// Only counted when the sandbox has a remote configured (a push was
	// actually attempted) — an unconfigured sandbox is not a failure.
	// Operators alert on outcome="failed" (a broken/expired token, a
	// missing host token while a remote is set, or a rejected push).
	GitPush = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_git_push_total",
		Help: "Auto-git-push attempts on task finish, by outcome.",
	}, []string{"outcome"}) // ok | failed
)

func init() {
	Registry.MustRegister(
		BuildInfo,
		SandboxesByStatus,
		APIRequests,
		APIDuration,
		DockerDuration,
		DockerErrors,
		ReconcilerRuns,
		ReconcilerLastDuration,
		ReconcilerOrphans,
		// Phase 5
		IdleReaperRuns,
		IdleReaperStops,
		PressureReaperRuns,
		PressureReaperStops,
		MemAvailablePercent,
		MemAvailableBytes,
		Wakes,
		WakeDuration,
		WakesRefusedActive,
		InflightExec,
		AccessLogLagSeconds,
		// nginx watcher
		NginxReloads,
		// Phase 6
		EgressConnections,
		EgressDrops,
		GitHostsRefreshRuns,
		AbuseListRefreshRuns,
		EgressLogLagSeconds,
		EgressSourcesActive,
		// Phase 7
		SnapshotsTaken,
		SnapshotRestores,
		SnapshotterRuns,
		SnapshotLastDurationSeconds,
		SnapshotLastSizeBytes,
		// Phase 9
		ForwardAuthDuration,
		PreviewAccess,
		// auto-git-push (NginxReloads is already registered above)
		GitPush,
	)
}

// RefreshSandboxGauge re-counts rows by status. Called from create /
// destroy paths and once at startup. Cheap — single COUNT GROUP BY.
func RefreshSandboxGauge(ctx context.Context, s *store.Store) error {
	rows, err := s.DB().QueryContext(ctx,
		`SELECT status, COUNT(*) FROM sandbox GROUP BY status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	// Reset known statuses to 0 first so a transition (e.g. last
	// 'creating' row leaves the table) sets the gauge back to zero
	// rather than reporting a stale value.
	for _, st := range []string{"creating", "running", "stopped", "error"} {
		SandboxesByStatus.WithLabelValues(st).Set(0)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return err
		}
		SandboxesByStatus.WithLabelValues(status).Set(float64(n))
	}
	return nil
}

// ObserveDocker is a tiny helper that records both duration and
// error count for a docker CLI invocation. Wrap the call with:
//
//	defer metrics.ObserveDocker("run", time.Now(), &err)
func ObserveDocker(op string, start time.Time, errOut *error) {
	DockerDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	if errOut != nil && *errOut != nil {
		DockerErrors.WithLabelValues(op).Inc()
	}
}
