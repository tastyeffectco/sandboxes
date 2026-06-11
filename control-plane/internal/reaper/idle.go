// Package reaper houses the two goroutines Phase 5 adds:
// the 30-second idle reaper and the 10-second host-memory-pressure
// reaper. Both call docker stop, both update SQLite via the existing
// single-writer channel from Phase 4, and both log every stop with a
// `reason` field.
//
// CLAUDE.md "Idle lifecycle" + "Host memory pressure reaper" specify
// the exact thresholds and bands. The idle reaper's *skip* rules
// come from CLAUDE.md "Activity definition" + roadmap §4 step 4.
//
// **OB9 carry-forward from Phase 1**: with memory.high=4G and
// memory.swap.max=0 the kernel can stall cooperative allocators in
// D-state before they ever cross memory.max=10G. The pressure
// reaper here does NOT rely on `memory.events.oom_kill` — it ranks
// rows by `last_active_at` for the 10-15% and 5-10% bands, and by
// `cgroup.memory.current` (RSS, NOT OOM events) for the <5%
// emergency branch. A D-state-stalled allocator counts as a kill
// candidate purely on those signals.
package reaper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sandboxd/control-plane/internal/activity"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// IdleConfig captures the env-tunable knobs from roadmap §12. Defaults
// come from CLAUDE.md "Idle lifecycle".
type IdleConfig struct {
	Threshold time.Duration // SANDBOXD_IDLE_THRESHOLD_SECONDS (600s)
	Interval  time.Duration // SANDBOXD_IDLE_REAP_INTERVAL_SECONDS (30s)
	WakeGrace time.Duration // SANDBOXD_WAKE_GRACE_SECONDS (60s) — post-WS/SSE-disconnect grace
}

// Idle is the goroutine.
type Idle struct {
	Cfg      IdleConfig
	Store    *store.Store
	Docker   *docker.Client
	Inflight *activity.InflightExec
	Egress   *egress.Manager // Phase 6 — nil-safe
	Log      *slog.Logger
}

// Run blocks until ctx is cancelled. A zero or negative Interval
// disables the loop (rollback path from roadmap §"Risks").
func (i *Idle) Run(ctx context.Context) error {
	if i.Cfg.Interval <= 0 {
		i.Log.Info("idle reaper: disabled (interval <= 0)")
		return nil
	}
	if i.Cfg.Threshold <= 0 {
		i.Log.Warn("idle reaper: threshold <= 0; using 600s default")
		i.Cfg.Threshold = 10 * time.Minute
	}
	if i.Cfg.WakeGrace <= 0 {
		i.Cfg.WakeGrace = 60 * time.Second
	}

	tk := time.NewTicker(i.Cfg.Interval)
	defer tk.Stop()
	for {
		if err := i.tick(ctx); err != nil && ctx.Err() == nil {
			i.Log.Warn("idle reaper: tick errored", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
	}
}

func (i *Idle) tick(ctx context.Context) error {
	metrics.IdleReaperRuns.Inc()
	now := time.Now().UTC()
	cutoff := now.Add(-i.Cfg.Threshold)

	candidates, err := i.Store.ListIdleCandidates(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("list idle candidates: %w", err)
	}
	for _, sb := range candidates {
		// Skip rule 1: in-flight exec.
		if i.Inflight != nil && i.Inflight.Active(sb.ID) {
			continue
		}
		// Skip rule 2: explicit keepalive.
		if sb.KeepaliveUntil.Valid && sb.KeepaliveUntil.Int64 > now.Unix() {
			continue
		}
		// Skip rule 2b: an active coding task — reaping mid-task would
		// interrupt the agent run.
		if has, err := i.Store.SandboxHasRunningTask(ctx, sb.ID); err != nil {
			i.Log.Warn("idle reaper: running-task check failed (continuing)",
				"sandbox_id", sb.ID, "err", err.Error())
		} else if has {
			i.Log.Info("idle reaper: skipping — active task", "sandbox_id", sb.ID)
			continue
		}
		// Skip rule 3 (the open-connection signal) is handled by the
		// access-log tailer / poller bumping last_active_at within
		// the WakeGrace window. The DB already reflects it via the
		// ListIdleCandidates cutoff comparison, so there is no extra
		// check needed here in the gated flow.
		// (If the poller is in fallback mode, SANDBOXD_WAKE_GRACE_SECONDS
		// is widened so that silent long-lived WS messages stay in the
		// cutoff window between message-driven access-log bumps.)

		// Skip rule 4 already encoded in the SQL ORDER BY — but
		// double-check for safety in case clock drift moved cutoff.
		if !sb.LastActiveAt.IsZero() && sb.LastActiveAt.After(cutoff) {
			continue
		}

		// Stop.
		log := i.Log.With(
			"sandbox_id", sb.ID,
			"reason", "idle",
			"deciding_signal", "last_active_at",
			"last_active_at", sb.LastActiveAt.Format(time.RFC3339),
		)
		log.Info("idle reaper: stopping sandbox")

		stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := i.Docker.Stop(stopCtx, "s-"+sb.ID, 10)
		cancel()
		if err != nil {
			log.Warn("idle reaper: docker stop failed", "err", err.Error())
			continue
		}
		// Phase 6 — drop the stopped container's bridge IP from
		// sandbox_sources_v4 + clear container_ip on the row. Best-
		// effort: a stopped container with a stale set entry is
		// harmless (no packets coming out anyway).
		if i.Egress != nil && sb.ContainerIP.Valid {
			if err := i.Egress.Remove(ctx, sb.ID, sb.ContainerIP.String); err != nil {
				log.Warn("idle reaper: egress.Remove failed (continuing)",
					"err", err.Error())
			}
			_ = i.Store.ClearContainerIP(ctx, sb.ID)
		}
		if err := i.Store.MarkStoppedAt(ctx, sb.ID, now); err != nil {
			log.Warn("idle reaper: MarkStoppedAt failed", "err", err.Error())
			continue
		}
		metrics.IdleReaperStops.WithLabelValues("idle").Inc()
	}
	return nil
}
