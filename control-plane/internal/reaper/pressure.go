package reaper

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sandboxd/control-plane/internal/activity"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/metrics"
	"github.com/sandboxd/control-plane/internal/store"
)

// PressureConfig captures the env-tunable knobs from roadmap §12.
// Defaults come from CLAUDE.md "Host memory pressure reaper".
type PressureConfig struct {
	Interval     time.Duration // SANDBOXD_PRESSURE_INTERVAL_SECONDS (10s)
	HeadroomPct  float64       // SANDBOXD_MEM_HEADROOM_PCT          (15)
	RefuseWakesPct float64     // SANDBOXD_MEM_REFUSE_WAKES_PCT      (10)
	EmergencyPct float64       // SANDBOXD_MEM_EMERGENCY_PCT         (5)
}

// Pressure is the goroutine.
type Pressure struct {
	Cfg      PressureConfig
	Store    *store.Store
	Docker   *docker.Client
	Inflight *activity.InflightExec
	Egress   *egress.Manager // Phase 6 — nil-safe
	Refused  *atomic.Bool // shared with the wake admission code; set when in 5-10% or <5% band
	Log      *slog.Logger
}

// Run blocks until ctx is cancelled. Cfg.Interval<=0 disables the loop
// (rollback path from roadmap §"Risks").
func (p *Pressure) Run(ctx context.Context) error {
	if p.Cfg.Interval <= 0 {
		p.Log.Info("pressure reaper: disabled (interval <= 0)")
		return nil
	}
	p.applyDefaults()

	tk := time.NewTicker(p.Cfg.Interval)
	defer tk.Stop()
	for {
		p.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
	}
}

func (p *Pressure) applyDefaults() {
	if p.Cfg.HeadroomPct <= 0 {
		p.Cfg.HeadroomPct = 15
	}
	if p.Cfg.RefuseWakesPct <= 0 {
		p.Cfg.RefuseWakesPct = 10
	}
	if p.Cfg.EmergencyPct <= 0 {
		p.Cfg.EmergencyPct = 5
	}
}

// Tick runs a single pressure-reaper iteration. Exported so the wake
// admission code can invoke it synchronously (roadmap §6).
func (p *Pressure) Tick(ctx context.Context) { p.tick(ctx) }

func (p *Pressure) tick(ctx context.Context) {
	metrics.PressureReaperRuns.Inc()
	mi, err := ReadMemInfo()
	if err != nil {
		p.Log.Warn("pressure reaper: read /proc/meminfo failed", "err", err.Error())
		return
	}
	avail := mi.AvailablePct()
	metrics.MemAvailablePercent.Set(avail)
	metrics.MemAvailableBytes.Set(float64(mi.AvailableBytes()))

	// Update the wake-refusal flag with hysteresis: we set on falling
	// edge into the 5-10% or <5% bands, clear on rising edge above
	// 12% (slightly above HeadroomPct's 10% trigger to avoid
	// flapping at the boundary).
	if p.Refused != nil {
		switch {
		case avail < p.Cfg.RefuseWakesPct:
			p.Refused.Store(true)
			metrics.WakesRefusedActive.Set(1)
		case avail >= p.Cfg.RefuseWakesPct+2:
			if p.Refused.Swap(false) {
				p.Log.Info("pressure reaper: wakes re-enabled", "avail_pct", avail)
			}
			metrics.WakesRefusedActive.Set(0)
		}
	}

	switch {
	case avail >= p.Cfg.HeadroomPct:
		// healthy
		return
	case avail >= p.Cfg.RefuseWakesPct:
		// 10-15% band — stop oldest idle-running
		p.stopOldestIdle(ctx, "10-15", avail)
	case avail >= p.Cfg.EmergencyPct:
		// 5-10% band — stop oldest idle-running AND warn loudly
		p.Log.Warn("pressure reaper: MemAvailable < 10% — wakes refused", "avail_pct", avail)
		p.stopOldestIdle(ctx, "5-10", avail)
	default:
		// <5% emergency
		p.Log.Error("pressure reaper: MemAvailable < 5% — EMERGENCY",
			"avail_pct", avail,
			"avail_bytes", mi.AvailableBytes(),
		)
		p.stopHeaviestRSS(ctx, "<5", avail)
	}
}

// stopOldestIdle picks the oldest idle-running sandbox (by
// last_active_at, ascending), applying the same skip rules as the
// idle reaper. If no candidate exists, the reaper takes no action
// this tick — we don't kill active work to defend an advisory
// threshold (CLAUDE.md "we don't kill active work to stay above an
// advisory threshold; that's reserved for <5%").
func (p *Pressure) stopOldestIdle(ctx context.Context, band string, availPct float64) {
	now := time.Now().UTC()
	// cutoff=now means "any row not currently being marked active";
	// the skip rules below handle inflight exec / keepalive.
	candidates, err := p.Store.ListIdleCandidates(ctx, now)
	if err != nil {
		p.Log.Warn("pressure reaper: list idle candidates failed", "err", err.Error())
		return
	}
	for _, sb := range candidates {
		if p.Inflight != nil && p.Inflight.Active(sb.ID) {
			continue
		}
		if sb.KeepaliveUntil.Valid && sb.KeepaliveUntil.Int64 > now.Unix() {
			continue
		}
		log := p.Log.With(
			"sandbox_id", sb.ID,
			"reason", "memory_pressure",
			"band", band,
			"avail_pct_before", availPct,
			"last_active_at", sb.LastActiveAt.Format(time.RFC3339),
		)
		log.Info("pressure reaper: stopping oldest idle")

		stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := p.Docker.Stop(stopCtx, "s-"+sb.ID, 10)
		cancel()
		if err != nil {
			log.Warn("pressure reaper: docker stop failed", "err", err.Error())
			return
		}
		// Phase 6 — drop the stopped container's bridge IP from the
		// egress sources set; clear container_ip on the row.
		if p.Egress != nil && sb.ContainerIP.Valid {
			if err := p.Egress.Remove(ctx, sb.ID, sb.ContainerIP.String); err != nil {
				log.Warn("pressure reaper: egress.Remove failed (continuing)",
					"err", err.Error())
			}
			_ = p.Store.ClearContainerIP(ctx, sb.ID)
		}
		if err := p.Store.MarkStoppedAt(ctx, sb.ID, now); err != nil {
			log.Warn("pressure reaper: MarkStoppedAt failed", "err", err.Error())
			return
		}
		metrics.PressureReaperStops.WithLabelValues(band).Inc()
		// One stop per tick is enough; the next tick re-reads
		// MemAvailable and decides again.
		return
	}
	p.Log.Debug("pressure reaper: no idle-running candidate this tick", "band", band, "avail_pct", availPct)
}

// stopHeaviestRSS picks the sandbox with the largest
// cgroup.memory.current and stops it regardless of activity.
// CLAUDE.md: "stop heaviest-RSS sandbox even if active". Logs a
// critical event with the full row identity.
func (p *Pressure) stopHeaviestRSS(ctx context.Context, band string, availPct float64) {
	now := time.Now().UTC()
	runs, err := p.Store.ListByStatuses(ctx, "running")
	if err != nil {
		p.Log.Warn("pressure reaper: ListByStatuses failed", "err", err.Error())
		return
	}
	var heaviest *struct {
		id, cgroup string
		rss        uint64
	}
	for _, sb := range runs {
		if !sb.CgroupPath.Valid {
			continue
		}
		rss, err := ReadCgroupMemoryCurrent(sb.CgroupPath.String)
		if err != nil {
			p.Log.Warn("pressure reaper: read memory.current failed",
				"sandbox_id", sb.ID, "err", err.Error())
			continue
		}
		if rss == 0 {
			continue
		}
		if heaviest == nil || rss > heaviest.rss {
			heaviest = &struct {
				id, cgroup string
				rss        uint64
			}{sb.ID, sb.CgroupPath.String, rss}
		}
	}
	if heaviest == nil {
		p.Log.Error("pressure reaper: <5% with no killable running cgroup — host is past help",
			"avail_pct", availPct)
		return
	}
	log := p.Log.With(
		"sandbox_id", heaviest.id,
		"reason", "memory_pressure_emergency",
		"band", band,
		"rss_bytes", heaviest.rss,
		"cgroup", heaviest.cgroup,
		"avail_pct_before", availPct,
	)
	log.Error("pressure reaper: EMERGENCY stopping heaviest-RSS sandbox")

	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = p.Docker.Stop(stopCtx, "s-"+heaviest.id, 10)
	cancel()
	if err != nil {
		log.Error("pressure reaper: emergency docker stop failed", "err", err.Error())
		return
	}
	// Phase 6 — drop the stopped container's bridge IP from the
	// egress sources set. We need to re-query the row here because
	// the loop above only carries heaviest's id+cgroup, not its
	// container_ip; fetch ipref by looking back into runs.
	if p.Egress != nil {
		for _, sb := range runs {
			if sb.ID == heaviest.id && sb.ContainerIP.Valid {
				if err := p.Egress.Remove(ctx, sb.ID, sb.ContainerIP.String); err != nil {
					log.Warn("pressure reaper: emergency egress.Remove failed (continuing)",
						"err", err.Error())
				}
				_ = p.Store.ClearContainerIP(ctx, sb.ID)
				break
			}
		}
	}
	if err := p.Store.MarkStoppedAt(ctx, heaviest.id, now); err != nil {
		log.Warn("pressure reaper: MarkStoppedAt failed", "err", err.Error())
		return
	}
	metrics.PressureReaperStops.WithLabelValues(band).Inc()
}
