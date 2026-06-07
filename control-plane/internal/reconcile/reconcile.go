// Package reconcile contains the boot-time reconciler. It runs once
// at startup, BEFORE the HTTP server begins accepting requests.
//
// CLAUDE.md non-negotiable #6: "SQLite is source of truth. The
// reconciler converges Docker to SQLite, never the other way."
// Phase 4 takes this literally: orphan containers and orphan mounts
// are LOGGED, not destroyed. A developer mid-debug who runs
// `docker run --name s-test ...` by hand loses nothing to this
// reconciler; multi-tenant cleanup is a Phase 8 problem.
//
// This reconciler also discharges the Phase 1 carry-forward
// "memory.high is not restored automatically by docker start" — for
// every running container, re-derive cgroup_path from the current
// pid and re-write memory.high. The write is idempotent.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/cgroup"
	"github.com/sandboxd/control-plane/internal/docker"
	"github.com/sandboxd/control-plane/internal/egress"
	"github.com/sandboxd/control-plane/internal/loopback"
	"github.com/sandboxd/control-plane/internal/snapshot"
	"github.com/sandboxd/control-plane/internal/store"
)

// Result is what Once returns to the caller (main.go logs + metrics).
type Result struct {
	Duration     time.Duration
	Rows         int
	Reapplied    int            // memory.high re-writes that succeeded
	Stopped      int            // rows we marked stopped because container vanished
	Errored      int            // rows we marked error
	EgressAdded  int            // Phase 6 — sandbox_sources_v4 entries re-installed
	Orphans      map[string]int // "container" / "mount" -> count
}

// Deps is the small set of collaborators the reconciler needs.
type Deps struct {
	Store    *store.Store
	Docker   *docker.Client
	Loopback *loopback.Manager
	Egress   *egress.Manager   // Phase 6 — nil-safe
	Snapshot *snapshot.Manager // Phase 7 — nil-safe
	Log      *slog.Logger

	// SetMemoryHigh gates the cgroup-v2 memory.high re-apply on boot.
	// Off in the portable OSS build (the control-plane container may
	// have no host cgroup access); when off the stored cgroup path is
	// preserved and the --memory ceiling still bounds the container.
	SetMemoryHigh bool
}

// Once runs the reconciler exactly once. Safe to call again only at
// process startup — there is intentionally no scheduling here (the
// idle / pressure reapers are Phase 5).
func Once(ctx context.Context, d Deps) (Result, error) {
	start := time.Now()
	res := Result{Orphans: map[string]int{"container": 0, "mount": 0, "workspace_owner": 0}}

	rows, err := d.Store.ListByStatuses(ctx, "running", "creating", "stopped")
	if err != nil {
		return res, fmt.Errorf("list sandboxes: %w", err)
	}
	res.Rows = len(rows)

	knownIDs := make(map[string]bool, len(rows))
	for _, sb := range rows {
		knownIDs[sb.ID] = true
		// Phase 5 V11 fix: kernel mount namespaces don't survive a
		// reboot; the .img on disk does. For every row whose .img is
		// still on disk, re-establish the loopback mount BEFORE the
		// row-level reconcile decides about container state. The wake
		// handler bind-mounts /var/lib/sandboxed/workspaces/
		// <id>.mnt into the container, and `docker start` fails with
		// `mkdir /home/sandbox/workspace: permission denied` if the
		// host-side mount is missing (empty .mnt dir). Provision() is
		// idempotent: a mount that already exists is a no-op; an
		// empty .mnt dir gets the loop device + mount re-established;
		// the seed gate stays closed because the .img already carries
		// the workspace contents. Skip rows in `error` status (we
		// don't auto-recover error rows; operator decides).
		if sb.Status != "error" && d.Loopback.ImgExists(sb.ID) {
			if err := d.Loopback.Provision(ctx, sb.ID); err != nil {
				d.Log.Warn("reconcile: re-mount loopback failed (continuing — wake will retry)",
					"sandbox_id", sb.ID, "err", err.Error())
			}
		}
		d.reconcileRow(ctx, sb, &res)
	}

	// Orphan-container scan: any "s-..." container without a row gets
	// LOGGED. We don't touch them — see package comment.
	names, err := d.Docker.ListByNamePrefix(ctx, "s-")
	if err != nil {
		d.Log.Warn("reconcile: list orphan-candidate containers failed", "err", err.Error())
	} else {
		for _, name := range names {
			id := strings.TrimPrefix(name, "s-")
			if knownIDs[id] {
				continue
			}
			d.Log.Warn("reconcile: orphan container (no matching sandbox row)",
				"container", name, "sandbox_id", id)
			res.Orphans["container"]++
		}
	}

	// Orphan-mount scan: any mount under workspaces/ without a row
	// gets logged.
	mounts, err := d.Loopback.ListMounts()
	if err != nil {
		d.Log.Warn("reconcile: list mounts failed", "err", err.Error())
	} else {
		for _, mnt := range mounts {
			// Filename shape: <Root>/<id>.mnt
			base := mnt
			if i := strings.LastIndexByte(mnt, '/'); i >= 0 {
				base = mnt[i+1:]
			}
			id := strings.TrimSuffix(base, ".mnt")
			if knownIDs[id] {
				continue
			}
			d.Log.Warn("reconcile: orphan mount (no matching sandbox row)",
				"mount", mnt, "sandbox_id", id)
			res.Orphans["mount"]++
		}
	}

	// Phase 7 — sweep snapshot crash debris: half-written
	// `*.img.zst.tmp` snapshots and `*.img.bak-*` files from an
	// interrupted restore. roadmap phase-7 §9 "Crash safety".
	if d.Snapshot != nil {
		tmp, bak := d.Snapshot.SweepStale()
		if tmp > 0 || bak > 0 {
			d.Log.Info("reconcile: swept snapshot debris",
				"stale_tmp", tmp, "stale_bak", bak)
		}
	}

	// Phase 8 — workspace_owner rows whose .img is gone are LOGGED,
	// never deleted (roadmap §13: "manual disposition").
	res.Orphans["workspace_owner"] = CheckWorkspaceOwnerOrphans(ctx, d.Store, d.Loopback, d.Log)

	res.Duration = time.Since(start)
	return res, nil
}

// CheckWorkspaceOwnerOrphans logs every workspace_owner row whose
// backing `.img` is no longer on disk and returns the count. It never
// deletes — disposition is the operator's (roadmap §13). Called once
// at boot from Once and on a 6 h ticker from main.
func CheckWorkspaceOwnerOrphans(ctx context.Context, st *store.Store, lb *loopback.Manager, log *slog.Logger) int {
	owners, err := st.ListAllWorkspaceOwners(ctx)
	if err != nil {
		log.Warn("reconcile: list workspace_owner failed", "err", err.Error())
		return 0
	}
	n := 0
	for _, wo := range owners {
		if !lb.ImgExists(wo.SandboxID) {
			log.Warn("reconcile: workspace_owner with no .img on disk (manual disposition)",
				"sandbox_id", wo.SandboxID, "external_user_id", wo.ExternalUserID)
			n++
		}
	}
	return n
}

func (d *Deps) reconcileRow(ctx context.Context, sb *store.Sandbox, res *Result) {
	log := d.Log.With("sandbox_id", sb.ID, "row_status", sb.Status)

	// Treat a "creating" row older than 5 minutes as failed. Roadmap
	// §"Risks": "Treat creating rows older than 5 minutes as failed:
	// log, mark error, leave the mount alone for human review."
	if sb.Status == "creating" && time.Since(sb.CreatedAt) > 5*time.Minute {
		log.Warn("reconcile: stale 'creating' row -> error (manual disposition)")
		if err := d.Store.MarkError(ctx, sb.ID, "interrupted while creating; reconciler timeout"); err != nil {
			log.Error("reconcile: MarkError failed", "err", err.Error())
		}
		res.Errored++
		return
	}

	name := "s-" + sb.ID
	cj, err := d.Docker.Inspect(ctx, name)
	if err != nil {
		// Not found → mark stopped (do NOT auto-recreate).
		if isNotFound(err) {
			log.Info("reconcile: container missing -> stopped")
			if err := d.Store.MarkStopped(ctx, sb.ID); err != nil {
				log.Error("reconcile: MarkStopped failed", "err", err.Error())
			}
			d.clearStoppedContainerIP(ctx, sb, log)
			res.Stopped++
			return
		}
		log.Error("reconcile: inspect failed", "err", err.Error())
		return
	}

	if !cj.State.Running {
		// Container exists but isn't running (exited).
		log.Info("reconcile: container present but not running -> stopped",
			"docker_state", cj.State.Status)
		if err := d.Store.MarkStopped(ctx, sb.ID); err != nil {
			log.Error("reconcile: MarkStopped failed", "err", err.Error())
		}
		d.clearStoppedContainerIP(ctx, sb, log)
		res.Stopped++
		return
	}

	// Re-derive cgroup_path and re-write memory.high (idempotent).
	// In the portable OSS build the cgroup write is disabled; preserve
	// the stored path and rely on the --memory ceiling. When enabled, a
	// write failure is logged but no longer marks the sandbox errored —
	// a running, serving container must not be torn out of service just
	// because a soft throttle couldn't be re-applied.
	rel := sb.CgroupPath.String
	if d.SetMemoryHigh {
		r2, err := cgroup.SetMemoryHigh(ctx, cj.State.Pid, sb.MemoryHigh)
		if err != nil {
			log.Warn("reconcile: re-apply memory.high failed (continuing)", "err", err.Error())
		} else {
			rel = r2
		}
	}
	short := cj.ID
	if len(short) > 12 {
		short = short[:12]
	}
	if err := d.Store.MarkRunning(ctx, sb.ID, short, rel); err != nil {
		log.Error("reconcile: MarkRunning failed", "err", err.Error())
		return
	}
	res.Reapplied++

	// Phase 8 §13 — a running sandbox should carry an external_user_id.
	// A missing one is impossible for a Phase-8-or-later create but
	// possible for a half-migrated legacy row. Log; never auto-destroy
	// (CLAUDE.md non-negotiable #6 — the reconciler does not adopt or
	// cull on its own).
	if !sb.ExternalUserID.Valid {
		log.Warn("reconcile: running sandbox has no external_user_id — run `sandboxd backfill-legacy`")
	}

	// Phase 6 — rebuild the dynamic sandbox_sources_v4 set from the
	// live bridge IP. Docker can reassign IPs across daemon restart,
	// so the stored container_ip on the row may be stale; the live
	// inspect IS the source of truth.
	//
	// roadmap §"Risks": "If sandboxd starts but the reconciler bails
	// before re-adding IPs, every running sandbox becomes unpoliced
	// until the reconciler completes." main.go enforces "reconciler
	// returns BEFORE HTTP listener accepts requests", so a partial
	// reconcile here just delays the listener — it never publishes a
	// half-populated set to clients.
	if d.Egress != nil {
		newIP := cj.NetworkSettings.IPAddress
		if newIP == "" {
			if n, ok := cj.NetworkSettings.Networks["bridge"]; ok && n != nil {
				newIP = n.IPAddress
			}
		}
		if newIP == "" {
			log.Warn("reconcile: no bridge IP for running sandbox — egress policy NOT installed",
				"docker_state", cj.State.Status)
		} else {
			// Tear down any stale entry first (the stored IP may
			// differ if docker reassigned).
			if sb.ContainerIP.Valid && sb.ContainerIP.String != "" && sb.ContainerIP.String != newIP {
				_ = d.Egress.Remove(ctx, sb.ID, sb.ContainerIP.String)
			}
			if err := d.Egress.Add(ctx, sb.ID, newIP); err != nil {
				log.Error("reconcile: egress.Add failed — refusing to leave sandbox unpoliced",
					"err", err.Error())
				if e2 := d.Store.MarkError(ctx, sb.ID,
					"reconciler: egress repopulation failed: "+err.Error()); e2 != nil {
					log.Error("reconcile: MarkError failed", "err", e2.Error())
				}
				res.Errored++
				return
			}
			if sb.ContainerIP.String != newIP {
				if err := d.Store.SetContainerIP(ctx, sb.ID, newIP); err != nil {
					log.Warn("reconcile: SetContainerIP failed (continuing)",
						"err", err.Error())
				}
			}
			res.EgressAdded++
		}
	}
}

// clearStoppedContainerIP NULLs container_ip on a row the reconciler
// just marked stopped. roadmap §1 DoD: "every stopped sandbox has it
// NULL". The idle / pressure reapers already ClearContainerIP on
// their stop path; the reconciler's mark-stopped path (containers
// gone after a host reboot) needs the same so a stale bridge IP
// doesn't linger on a stopped row. No-op when the column is already
// NULL or egress isn't wired.
func (d *Deps) clearStoppedContainerIP(ctx context.Context, sb *store.Sandbox, log *slog.Logger) {
	if !sb.ContainerIP.Valid {
		return
	}
	if err := d.Store.ClearContainerIP(ctx, sb.ID); err != nil {
		log.Warn("reconcile: ClearContainerIP failed (continuing)", "err", err.Error())
	}
}

func isNotFound(err error) bool {
	return err == docker.ErrNotFound
}
