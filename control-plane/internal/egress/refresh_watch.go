package egress

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/sandboxd/control-plane/internal/metrics"
)

// RefreshWatcher observes the two systemd oneshot units that refresh
// the nftables allow/deny sets (sandbox-git-hosts-refresh.service and
// sandbox-abuse-list-refresh.service) and feeds their outcomes into
// the `sandboxd_{git_hosts,abuse_list}_refresh_runs_total` counters.
//
// The refresh jobs are bash scripts driven by systemd timers — they
// cannot increment a Prometheus counter inside sandboxd directly.
// This watcher bridges the gap: it polls `systemctl show` for each
// unit, tracks the per-run `InvocationID` (systemd mints a fresh one
// every activation), and increments the counter with outcome `ok` or
// `failed` whenever a new InvocationID appears.
//
// roadmap/phase-6-hardening-and-egress.md §9 lists these two metrics
// but does not specify the collection mechanism; this watcher is the
// Phase 6 host-execution fix (report §"Issues encountered" Issue #1).
type RefreshWatcher struct {
	Interval time.Duration // poll cadence; default 60s
	Log      *slog.Logger

	// systemctlBin is injectable for tests; production uses "systemctl".
	systemctlBin string

	// last-seen InvocationID per unit. Empty until the first poll.
	lastGit   string
	lastAbuse string
	primed    bool
}

// Run blocks until ctx is cancelled. Interval<=0 → 60s default.
func (w *RefreshWatcher) Run(ctx context.Context) error {
	if w.Interval <= 0 {
		w.Interval = 60 * time.Second
	}
	if w.systemctlBin == "" {
		w.systemctlBin = "systemctl"
	}
	tk := time.NewTicker(w.Interval)
	defer tk.Stop()
	// Poll once immediately so the counters carry the install-time
	// run without waiting a full interval.
	w.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tk.C:
		}
		w.poll(ctx)
	}
}

func (w *RefreshWatcher) poll(ctx context.Context) {
	gitID, gitOK := w.unitState(ctx, "sandbox-git-hosts-refresh.service")
	abuseID, abuseOK := w.unitState(ctx, "sandbox-abuse-list-refresh.service")

	if gitID != "" && gitID != w.lastGit {
		metrics.GitHostsRefreshRuns.WithLabelValues(outcome(gitOK)).Inc()
		w.lastGit = gitID
	}
	if abuseID != "" && abuseID != w.lastAbuse {
		metrics.AbuseListRefreshRuns.WithLabelValues(outcome(abuseOK)).Inc()
		w.lastAbuse = abuseID
	}
	w.primed = true
}

// unitState returns the unit's current InvocationID and whether its
// last run succeeded. A oneshot unit reports Result=success on a
// clean exit; anything else (exit-code, timeout, signal) is a fail.
func (w *RefreshWatcher) unitState(ctx context.Context, unit string) (invocationID string, ok bool) {
	cmd := exec.CommandContext(ctx, w.systemctlBin, "show", unit,
		"--property=InvocationID", "--property=Result")
	out, err := cmd.Output()
	if err != nil {
		// Unit not installed yet / systemctl unavailable — stay quiet;
		// the next poll retries.
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	var result string
	for sc.Scan() {
		line := sc.Text()
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch k {
		case "InvocationID":
			invocationID = v
		case "Result":
			result = v
		}
	}
	return invocationID, result == "success"
}

func outcome(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}
