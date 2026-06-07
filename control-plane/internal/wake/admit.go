// Package wake owns the wake admission check and the /wake/{id}
// handler. CLAUDE.md "Wake admission" gives the exact formula; this
// package implements it once and is called by both POST /wake/{id}
// and POST /sandbox (create) so the floor is uniform.
package wake

import (
	"context"
	"sync/atomic"

	"github.com/sandboxd/control-plane/internal/reaper"
)

// AdmitConfig parametrises the admission function. Defaults mirror
// CLAUDE.md ("Wake admission" + "Resource policy"):
//
//	WakeCostMB     = 800   // estimated incremental RAM for one wake
//	FloorPct       = 10    // refuse below this (10% headroom)
//
// The reaper-driven `wakesRefused` flag is consulted via Refused; if
// non-nil and currently true, Admit returns Denied without invoking
// the synchronous pressure tick (saves a /proc/meminfo read in the
// hot path and avoids us stop-killing a sandbox during an
// already-known emergency).
type AdmitConfig struct {
	WakeCostMB uint64       // SANDBOXD_WAKE_COST_MB        (800)
	FloorPct   float64      // SANDBOXD_MEM_REFUSE_WAKES_PCT (10)
	Refused    *atomic.Bool // shared with the pressure reaper

	// Tick is the synchronous pressure-reaper invocation. Admit calls
	// it once between the first and second meminfo read. Nil disables
	// the synchronous retry — useful in tests.
	Tick func(ctx context.Context)
}

// Outcome is the result of an admission check.
type Outcome struct {
	Admit       bool
	AvailPct    float64
	AvailBytes  uint64
	Reason      string // populated when Admit==false
}

// Admit performs the CLAUDE.md "Wake admission" calculation:
//
//  1. read /proc/meminfo
//  2. cost_pct = WakeCostMB / total_mb * 100
//  3. if (avail_pct - cost_pct) >= FloorPct  → ok
//  4. else if Refused.Load() == true         → denied (no retry)
//  5. else run a synchronous pressure-reaper tick
//  6. re-read /proc/meminfo
//  7. if still below FloorPct                → denied
//  8. otherwise                              → ok
func Admit(ctx context.Context, cfg AdmitConfig) (Outcome, error) {
	cfg.applyDefaults()

	mi, err := reaper.ReadMemInfo()
	if err != nil {
		return Outcome{}, err
	}
	availPct := mi.AvailablePct()
	availBytes := mi.AvailableBytes()
	totalKB := mi.Total
	costPct := float64(cfg.WakeCostMB*1024) * 100.0 / float64(totalKB)
	if availPct-costPct >= cfg.FloorPct {
		return Outcome{Admit: true, AvailPct: availPct, AvailBytes: availBytes}, nil
	}
	if cfg.Refused != nil && cfg.Refused.Load() {
		return Outcome{
			Admit:      false,
			AvailPct:   availPct,
			AvailBytes: availBytes,
			Reason:     "wakes_refused",
		}, nil
	}

	// Synchronous reaper tick — may stop an idle sandbox.
	if cfg.Tick != nil {
		cfg.Tick(ctx)
	}

	mi2, err := reaper.ReadMemInfo()
	if err != nil {
		return Outcome{}, err
	}
	availPct = mi2.AvailablePct()
	availBytes = mi2.AvailableBytes()
	if availPct-costPct >= cfg.FloorPct {
		return Outcome{Admit: true, AvailPct: availPct, AvailBytes: availBytes}, nil
	}
	return Outcome{
		Admit:      false,
		AvailPct:   availPct,
		AvailBytes: availBytes,
		Reason:     "low_memory",
	}, nil
}

func (c *AdmitConfig) applyDefaults() {
	if c.WakeCostMB == 0 {
		c.WakeCostMB = 800
	}
	if c.FloorPct == 0 {
		c.FloorPct = 10
	}
}
